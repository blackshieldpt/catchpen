// Package qemu builds QEMU command lines and talks QMP.
package qemu

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/egress"
)

type Forward struct{ Host, Guest int }

type Spec struct {
	Name     string
	CPUs     int
	MemoryMB int64
	Disk     string
	Seed     string
	SSHPort  int // 0 = no SSH forward (build VMs)
	Forwards []Forward
	// Egress: egress.ModeAllowlist (restrict + proxy guestfwd), ModeNone (restrict, no exit), ModeOpen.
	Egress     string
	ProxyCmd   []string // argv spawned by QEMU per guest proxy connection
	ProxyIP    string
	ProxyPort  int
	NotifyCmd  []string // argv spawned per connection to NotifyAddr (agent notifications)
	NotifyIP   string
	NotifyPort int

	QMP        string // unix socket path
	Serial     string // unix socket path (ignored when SerialFile is set)
	SerialLog  bool   // also log the Serial socket to <Serial>.log (unbounded; debug only)
	SerialFile string
	LogFile    string // second serial port (ttyS1) to a file; build log
	Pidfile    string
}

// esc escapes a value embedded in a QEMU comma-separated option string.
func esc(s string) string { return strings.ReplaceAll(s, ",", ",,") }

// shellQuote quotes argv for libslirp's guestfwd cmd: (parsed with shell rules).
func shellQuote(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func (s Spec) netdev() (string, error) {
	opts := []string{"user", "id=n0", "ipv6=off"}
	switch s.Egress {
	case egress.ModeAllowlist, egress.ModeNone:
		opts = append(opts, "restrict=on")
	case egress.ModeOpen:
	default:
		return "", fmt.Errorf("unknown egress mode %q", s.Egress)
	}
	if s.SSHPort > 0 {
		opts = append(opts, fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", s.SSHPort))
	}
	for _, f := range s.Forwards {
		opts = append(opts, fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", f.Host, f.Guest))
	}
	if s.Egress == egress.ModeAllowlist {
		if len(s.ProxyCmd) == 0 {
			return "", errors.New("allowlist egress needs a proxy command")
		}
		opts = append(opts, fmt.Sprintf("guestfwd=tcp:%s:%d-cmd:%s", s.ProxyIP, s.ProxyPort, esc(shellQuote(s.ProxyCmd))))
	}
	if len(s.NotifyCmd) > 0 {
		opts = append(opts, fmt.Sprintf("guestfwd=tcp:%s:%d-cmd:%s", s.NotifyIP, s.NotifyPort, esc(shellQuote(s.NotifyCmd))))
	}
	return strings.Join(opts, ","), nil
}

// Args returns the qemu-system-x86_64 argv (without the binary).
func (s Spec) Args() ([]string, error) {
	nd, err := s.netdev()
	if err != nil {
		return nil, err
	}
	a := []string{
		"-name", "curral-" + s.Name,
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", fmt.Sprintf("%dM", s.MemoryMB),
		"-drive", "file=" + esc(s.Disk) + ",if=virtio,format=qcow2,discard=unmap,cache=writeback",
		"-netdev", nd,
		"-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
		"-display", "none",
		"-nodefaults",
		"-vga", "std", // Debian cloud images don't boot without a video device
		// seccomp. The filter is inherited by the guestfwd proxy (curral egress-stdio),
		// so spawn stays allowed and elevateprivileges=deny is omitted: it kills the Go child.
		"-sandbox", "on,obsolete=deny,resourcecontrol=deny",
	}
	if s.Seed != "" {
		a = append(a, "-drive", "file="+esc(s.Seed)+",media=cdrom,readonly=on,format=raw")
	}
	if s.QMP != "" {
		a = append(a, "-qmp", "unix:"+esc(s.QMP)+",server=on,wait=off")
	}
	switch {
	case s.SerialFile != "":
		a = append(a, "-serial", "file:"+s.SerialFile)
	case s.Serial != "":
		cd := "socket,id=ser0,path=" + esc(s.Serial) + ",server=on,wait=off"
		if s.SerialLog {
			cd += ",logfile=" + esc(s.Serial+".log") + ",logappend=off"
		}
		a = append(a, "-chardev", cd, "-serial", "chardev:ser0")
	}
	if s.LogFile != "" {
		a = append(a, "-serial", "file:"+s.LogFile)
	}
	if s.Pidfile != "" {
		a = append(a, "-pidfile", s.Pidfile)
	}
	return a, nil
}

// Launch starts QEMU detached in its own session (QEMU's -daemonize needs
// setsid, which the seccomp sandbox denies). It returns once the QMP socket
// accepts connections, or an error if QEMU exits first.
func Launch(s Spec, dir string, log *os.File) (int, error) {
	if s.QMP == "" {
		return 0, errors.New("launch needs a QMP socket")
	}
	bin, err := Binary()
	if err != nil {
		return 0, err
	}
	args, err := s.Args()
	if err != nil {
		return 0, err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case err := <-exited:
			return 0, fmt.Errorf("qemu exited during startup: %w", err)
		case <-deadline:
			_ = cmd.Process.Kill()
			return 0, errors.New("qemu did not open its QMP socket within 30s")
		case <-time.After(100 * time.Millisecond):
		}
		if c, err := net.Dial("unix", s.QMP); err == nil {
			c.Close()
			return cmd.Process.Pid, nil
		}
	}
}

// Binary returns the qemu system emulator path.
func Binary() (string, error) { return exec.LookPath("qemu-system-x86_64") }

// Pid reads a pidfile and reports whether that process is the QEMU started for
// VM name (checked via its -name argument, so a reused PID isn't mistaken for it).
func Pid(pidfile, name string) (int, bool) {
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return pid, false
	}
	b, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return pid, false
	}
	argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if !strings.HasPrefix(filepath.Base(argv[0]), "qemu-system") {
		return pid, false
	}
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "-name" && argv[i+1] == "curral-"+name {
			return pid, true
		}
	}
	return pid, false
}

// QMP is a minimal QMP client (one command at a time).
type QMP struct {
	conn net.Conn
	r    *bufio.Reader
}

func DialQMP(path string, timeout time.Duration) (*QMP, error) {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	q := &QMP{conn: c, r: bufio.NewReader(c)}
	if _, err := q.r.ReadBytes('\n'); err != nil { // greeting
		c.Close()
		return nil, err
	}
	if _, err := q.Execute("qmp_capabilities", nil); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return q, nil
}

func (q *QMP) Close() error { return q.conn.Close() }

// Execute runs a command and returns its "return" payload, skipping async events.
func (q *QMP) Execute(cmd string, args map[string]any) (json.RawMessage, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	_ = q.conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer q.conn.SetDeadline(time.Time{})
	if _, err := q.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			if cmd == "quit" && len(line) == 0 {
				return nil, nil
			}
			return nil, err
		}
		var resp struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			} `json:"error"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, err
		}
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", cmd, resp.Error.Class, resp.Error.Desc)
		}
		return resp.Return, nil
	}
}

// FreePort asks the kernel for an unused loopback TCP port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// CreateOverlay creates a qcow2 overlay on top of backing (absolute path).
func CreateOverlay(path, backing string, sizeMB int64) error {
	args := []string{"create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", backing, path}
	if sizeMB > 0 {
		args = append(args, fmt.Sprintf("%dM", sizeMB))
	}
	return qemuImg(args...)
}

// Convert makes a standalone qcow2 copy of src resized to sizeMB. The source
// format is probed once and pinned; images that reference other files (a
// backing file or a qcow2 external data file) are refused, since qemu-img
// would read those host paths into the output.
func Convert(src, dst string, sizeMB int64) error {
	out, err := exec.Command("qemu-img", "info", "--output=json", "-U", src).Output()
	if err != nil {
		return fmt.Errorf("qemu-img info %s: %w", src, err)
	}
	var info struct {
		Format         string `json:"format"`
		BackingFile    string `json:"backing-filename"`
		FormatSpecific struct {
			Data struct {
				DataFile    string `json:"data-file"`
				DataFileRaw bool   `json:"data-file-raw"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return err
	}
	if info.BackingFile != "" {
		return fmt.Errorf("%s references a backing file; refusing", src)
	}
	if d := info.FormatSpecific.Data; d.DataFile != "" || d.DataFileRaw {
		return fmt.Errorf("%s references an external data file; refusing", src)
	}
	if info.Format != "qcow2" && info.Format != "raw" {
		return fmt.Errorf("%s: unsupported image format %q", src, info.Format)
	}
	if err := qemuImg("convert", "-q", "-f", info.Format, "-O", "qcow2", src, dst); err != nil {
		return err
	}
	return qemuImg("resize", "-q", dst, fmt.Sprintf("%dM", sizeMB))
}

// VirtualSizeMB returns a disk image's virtual size.
func VirtualSizeMB(path string) (int64, error) {
	out, err := exec.Command("qemu-img", "info", "--output=json", "-U", path).Output()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, err
	}
	return info.VirtualSize >> 20, nil
}

func qemuImg(args ...string) error {
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
