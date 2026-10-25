package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/blackshieldpt/curral/internal/paths"
)

func consoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "console <name>",
		Short: "Attach to a VM's serial console (boot log; Ctrl-] to detach)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if _, err := m.SSHPort(args[0]); err != nil {
				return err
			}
			conn, err := net.Dial("unix", m.SerialSocket(args[0]))
			if err != nil {
				return err
			}
			defer conn.Close()
			fmt.Fprintln(os.Stderr, "connected to serial console; Ctrl-] to detach (no password login: use curral ssh)")
			if term.IsTerminal(int(os.Stdin.Fd())) {
				st, err := term.MakeRaw(int(os.Stdin.Fd()))
				if err != nil {
					return err
				}
				defer term.Restore(int(os.Stdin.Fd()), st)
			}
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(&termFilter{w: os.Stdout}, conn); done <- struct{}{} }()
			go func() {
				buf := make([]byte, 256)
				for {
					n, err := os.Stdin.Read(buf)
					if i := bytes.IndexByte(buf[:n], 0x1d); i >= 0 {
						_, _ = conn.Write(buf[:i])
						break
					}
					if n > 0 {
						if _, err := conn.Write(buf[:n]); err != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
				done <- struct{}{}
			}()
			<-done
			fmt.Fprint(os.Stderr, "\r\ndetached\r\n")
			return nil
		},
	}
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host requirements",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			failed := false
			check := func(name string, err error, hint string) {
				if err != nil {
					failed = true
					fmt.Printf("FAIL  %-22s %v\n      %s\n", name, err, hint)
					return
				}
				fmt.Printf("ok    %s\n", name)
			}
			warn := func(name, msg string) { fmt.Printf("warn  %-22s %s\n", name, msg) }

			for _, b := range []struct{ bin, hint string }{
				{"qemu-system-x86_64", "sudo apt install qemu-system-x86"},
				{"qemu-img", "sudo apt install qemu-utils"},
				{"ssh", "sudo apt install openssh-client"},
			} {
				_, err := exec.LookPath(b.bin)
				check(b.bin, err, b.hint)
			}
			f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
			if err == nil {
				f.Close()
			}
			check("/dev/kvm read/write", err, "sudo usermod -aG kvm $USER, then log in again")
			if err == nil && !inGroup("kvm") {
				warn("kvm group", "access comes from a login-session ACL only; services and ssh sessions may lack it (sudo usermod -aG kvm $USER)")
			}
			m, err := manager()
			check("config", err, "fix "+paths.Default().ConfigFile())
			if err == nil {
				check("data dir", m.P.Ensure(), "check permissions of "+m.P.Data)
				if strings.HasPrefix(m.Bin, os.TempDir()) {
					warn("curral binary", "running from a temp dir (go run?); install it so QEMU and ssh can call it: "+m.Bin)
				}
			}
			if _, err := exec.LookPath("notify-send"); err != nil {
				warn("notify-send", "not found; `egress watch --desktop` unavailable (sudo apt install libnotify-bin)")
			}
			if _, err := exec.LookPath("ssh-add"); err != nil {
				warn("ssh-agent", "ssh-agent/ssh-add not found; ssh.agent: restricted unavailable")
			} else if p := askpassProblem(); p != "" {
				warn("ssh-askpass", p)
			}
			if failed {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
}

func inGroup(name string) bool {
	g, err := user.LookupGroup(name)
	if err != nil {
		return false
	}
	ids, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, id := range ids {
		if fmt.Sprint(id) == g.Gid {
			return true
		}
	}
	return false
}

// askpassProblem says why ssh-agent can't ask for key-use confirmation, or "" if it can.
func askpassProblem() string {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "no display; restricted agents with confirm on can't ask (on a headless host, set confirm: false)"
	}
	if p := os.Getenv("SSH_ASKPASS"); p != "" {
		if _, err := exec.LookPath(p); err != nil {
			return "SSH_ASKPASS " + p + " not found; restricted agents with confirm on can't ask"
		}
		return ""
	}
	if _, err := os.Stat("/usr/bin/ssh-askpass"); err != nil {
		return "no askpass program; restricted agents with confirm on can't ask (sudo apt install ssh-askpass, or set SSH_ASKPASS)"
	}
	return ""
}
