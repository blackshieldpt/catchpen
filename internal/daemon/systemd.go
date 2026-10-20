package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

const unitName = "curral.service"

// UnitPath is where the systemd user unit goes: a directory systemd always
// searches, whatever XDG_CONFIG_HOME its manager has.
func UnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", unitName)
}

// Unit renders the unit running bin as the daemon. The variables that pick
// curral's directories are carried over, so it sees the same VMs, and PATH,
// so it finds the same qemu.
func Unit(bin string, env func(string) string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=curral daemon: warm pool, idle stop, base rebuilds, egress prompts\n\n[Service]\n")
	if v := env("PATH"); v != "" {
		fmt.Fprintf(&b, "Environment=%s\n", unitQuote("PATH="+v))
	}
	for _, k := range []string{"CURRAL_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		if v := env(k); v != "" && filepath.IsAbs(v) {
			fmt.Fprintf(&b, "Environment=%s\n", unitQuote(k+"="+v))
		}
	}
	// ExecStart (unlike Environment) also expands $VARIABLES.
	fmt.Fprintf(&b, "ExecStart=%s daemon\nRestart=on-failure\nRestartSec=30\n\n[Install]\nWantedBy=default.target\n", strings.ReplaceAll(unitQuote(bin), "$", "$$"))
	return b.String()
}

// unitQuote quotes a word for a unit file: specifiers (%) and quotes escaped.
func unitQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Install writes the unit, then enables and (re)starts it.
func Install(bin string) error {
	p := UnitPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(p, []byte(Unit(bin, os.Getenv)), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", unitName); err != nil {
		return err
	}
	return systemctl("restart", unitName)
}

// Uninstall stops and disables the unit and removes it.
func Uninstall() error {
	p := UnitPath()
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("%s is not installed", p)
	}
	if err := systemctl("disable", "--now", unitName); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	return systemctl("daemon-reload")
}

// UnitState is systemd's view of the unit ("" when not installed).
func UnitState() string {
	if _, err := os.Stat(UnitPath()); err != nil {
		return ""
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", unitName).Output()
	return strings.TrimSpace(string(out))
}
