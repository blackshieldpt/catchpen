package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// egress config reports refused settings with its flag names, and keeps its
// order: the VM, its mode and its egress.json are checked before the flags.
func TestEgressConfigErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CATCHPEN_HOME", home)
	for name, egressJSON := range map[string]string{"a": "", "broken": "{"} {
		dir := filepath.Join(home, "data", "vms", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(`{"name":"`+name+`","egress":"allowlist"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if egressJSON != "" {
			if err := os.WriteFile(filepath.Join(dir, "egress.json"), []byte(egressJSON), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"config", "a", "--on-deny", "ask"}, "--on-deny must be log or prompt"},
		{[]string{"config", "a", "--on-deny="}, "--on-deny must be log or prompt"},
		{[]string{"config", "a", "--prompt-timeout", "xyz"}, `--prompt-timeout: invalid duration "xyz"`},
		{[]string{"config", "a", "--prompt-timeout", "-5s"}, `--prompt-timeout: invalid duration "-5s"`},
		{[]string{"config", "a", "--on-deny", "ask", "--prompt-timeout", "xyz"}, "--on-deny must be log or prompt"},
		{[]string{"config", "broken", "--on-deny", "ask"}, "egress.json: unexpected end of JSON input"},
	} {
		cmd := egressCmd()
		cmd.SetArgs(c.args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}
