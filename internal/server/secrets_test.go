package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/creds"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// noDecoy fails t if the decoy is in the server's log or the audit log.
func noDecoy(t *testing.T, h *vmHost) {
	t.Helper()
	h.mu.Lock()
	logged := h.log.String()
	h.mu.Unlock()
	b, _ := os.ReadFile(h.p.Audit())
	if strings.Contains(logged, decoy) || strings.Contains(string(b), decoy) {
		t.Errorf("a value logged:\n%s\n%s", logged, b)
	}
}

func TestSecrets(t *testing.T) {
	var h *vmHost
	h = home(t, func() (*vm.Manager, error) {
		cfg := config.Default()
		cfg.Secrets = map[string]creds.Source{"SHADOWED": {Env: "ELSEWHERE"}}
		return &vm.Manager{P: h.p, Cfg: cfg, Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	c := h.client(t)
	ctx := context.Background()
	path := h.p.SecretsFile()
	if ss, err := c.Secrets(ctx); err != nil || ss == nil || len(ss) != 0 {
		t.Fatalf("no file: %+v %v", ss, err)
	}

	ss, err := c.SetSecret(ctx, "GH_TOKEN", decoy, false)
	if err != nil || fmt.Sprint(ss) != "[{GH_TOKEN false}]" {
		t.Fatalf("set: %+v %v", ss, err)
	}
	fileIs(t, path, "GH_TOKEN="+decoy+"\n", 0o600)
	if ev := lastEvent(t, h.p, audit.SecretSet); ev.Detail["name"] != "GH_TOKEN" || ev.Detail["replaced"] != false || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}
	_, err = c.SetSecret(ctx, "GH_TOKEN", "other", false)
	refused(t, "replaced", err, api.CodeConfirmRequired, `secrets.env sets GH_TOKEN already, and its value would be lost: send "confirm": true`)
	fileIs(t, path, "GH_TOKEN="+decoy+"\n", 0o600)

	// A file as catchpen init writes it: a placeholder is filled in, every
	// other line kept, and so is the mode.
	write(t, path, "## tokens\n# GH_TOKEN=\n# NTFY_TOKEN=\nGH_TOKEN="+decoy+"\nEMPTY=\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetSecret(ctx, "NTFY_TOKEN", "n", false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetSecret(ctx, "GH_TOKEN", "new", true); err != nil {
		t.Fatal(err)
	}
	fileIs(t, path, "## tokens\n# GH_TOKEN=\nNTFY_TOKEN=n\nGH_TOKEN=new\nEMPTY=\n", 0o700)
	if ev := lastEvent(t, h.p, audit.SecretSet); ev.Detail["name"] != "GH_TOKEN" || ev.Detail["replaced"] != true {
		t.Errorf("audit: %+v", ev)
	}
	// EMPTY= sets nothing; a config.yaml source shadows the file.
	if ss, err = c.SetSecret(ctx, "SHADOWED", "s", false); err != nil || fmt.Sprint(ss) != "[{GH_TOKEN false} {NTFY_TOKEN false} {SHADOWED true}]" {
		t.Errorf("listed: %+v %v", ss, err)
	}

	_, err = c.UnsetSecret(ctx, "NOSUCH", true)
	refused(t, "unset NOSUCH", err, api.CodeNotFound, "secrets.env doesn't set NOSUCH")
	_, err = c.UnsetSecret(ctx, "GH_TOKEN", false)
	refused(t, "no confirm", err, api.CodeConfirmRequired, `removing GH_TOKEN from secrets.env can't be undone: send "confirm": true`)
	if ss, err = c.UnsetSecret(ctx, "GH_TOKEN", true); err != nil || fmt.Sprint(ss) != "[{NTFY_TOKEN false} {SHADOWED true}]" {
		t.Errorf("unset: %+v %v", ss, err)
	}
	fileIs(t, path, "## tokens\n# GH_TOKEN=\nNTFY_TOKEN=n\nEMPTY=\nSHADOWED=s\n", 0o700)
	if ev := lastEvent(t, h.p, audit.SecretUnset); ev.Detail["name"] != "GH_TOKEN" || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}

	// The fields, by their wire names; no answer holds a value.
	code, body := h.post(t, api.PathSecret("WIRE"), `{"value":"`+decoy+`"}`)
	if code != 200 || !strings.Contains(body, `{"name":"WIRE","shadowed":false}`) || !strings.Contains(body, `{"name":"SHADOWED","shadowed":true}`) || strings.Contains(body, decoy) {
		t.Errorf("set on the wire: %d %s", code, body)
	}
	if code, body := h.post(t, api.PathSecret("WIRE"), `{"value":"x","confirm":true}`); code != 200 || strings.Contains(body, decoy) {
		t.Errorf("replaced on the wire: %d %s", code, body)
	}
	if code, body := h.raw(t, api.PathSecrets); code != 200 || !strings.Contains(body, `"name":"WIRE"`) {
		t.Errorf("listed on the wire: %d %s", code, body)
	}
	if code, body := h.post(t, api.PathSecretDelete("WIRE"), `{"confirm":true}`); code != 200 || strings.Contains(body, "WIRE") {
		t.Errorf("unset on the wire: %d %s", code, body)
	}

	// A value on an earlier line is lost by a set too; a token pasted on a
	// line of its own isn't listed as a name, nor is a name the API can't
	// set; an unset removes a NAME= line.
	write(t, path, "DUP="+decoy+"\nDUP=\nEMPTY=\n"+decoy+"==\n"+decoy+"+/x==\nmy/key=v\n")
	if code, body := h.raw(t, api.PathSecrets); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("listed: %d %s", code, body)
	}
	_, err = c.SetSecret(ctx, "DUP", "v", false)
	refused(t, "an earlier value", err, api.CodeConfirmRequired, "secrets.env sets DUP already")
	if ss, err = c.UnsetSecret(ctx, "DUP", true); err != nil || len(ss) != 0 {
		t.Errorf("unset DUP: %+v %v", ss, err)
	}
	_, err = c.UnsetSecret(ctx, "EMPTY", false)
	refused(t, "unset EMPTY", err, api.CodeConfirmRequired, "removing EMPTY from secrets.env")
	if _, err = c.UnsetSecret(ctx, "EMPTY", true); err != nil {
		t.Errorf("unset EMPTY: %v", err)
	}
	fileIs(t, path, decoy+"==\n"+decoy+"+/x==\nmy/key=v\n", 0o700)
	if n, m := len(events(t, h, audit.SecretSet)), len(events(t, h, audit.SecretUnset)); n != 6 || m != 4 {
		t.Errorf("%d sets and %d unsets audited", n, m)
	}
	noDecoy(t, h)
}

func TestSecretsRefused(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	ctx := context.Background()
	path := h.p.SecretsFile()

	const badName = "a secret's name is letters, digits, _, . and -, not starting with a digit, . or -, up to 128"
	for _, tc := range []struct{ what, name, value, msg string }{
		{"a digit first", "9X", "v", badName},
		{"a dot first", ".env", "v", badName},
		{"a slash", "A/B", "v", badName},
		{"too long a name", strings.Repeat("A", 129), "v", badName},
		{"no value", "K", "", "the value is empty: unset the secret instead"},
		{"a newline", "K", decoy + "\nB=2", "the value contains a newline, CR or NUL"},
		{"a NUL", "K", "a\x00b", "the value contains a newline, CR or NUL"},
		{"too long a value", "K", strings.Repeat("x", creds.MaxValue+1), "the value is longer than 32768 bytes"},
	} {
		_, err := c.SetSecret(ctx, tc.name, tc.value, true)
		refused(t, tc.what, err, api.CodeBadRequest, tc.msg)
		if err != nil && strings.Contains(err.Error(), decoy) {
			t.Errorf("%s: %v", tc.what, err)
		}
	}
	_, err := c.UnsetSecret(ctx, "A/B", true)
	refused(t, "unset a slash", err, api.CodeBadRequest, badName)
	if code, body := h.post(t, api.PathSecret("K"), `{"value":"v","mode":"0777"}`); code != 400 || !strings.Contains(body, "invalid request body") {
		t.Errorf("an unknown field: %d %s", code, body)
	}
	if code, body := h.post(t, api.PathSecretDelete("K"), `{"confirm":true,"value":""}`); code != 400 || !strings.Contains(body, "invalid request body") {
		t.Errorf("an unknown unset field: %d %s", code, body)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("written")
	}

	// A file catchpen can't read is fixed by hand.
	for _, bad := range []struct {
		what, text string
		mode       os.FileMode
	}{{"readable by others", "K=" + decoy + "\n", 0o640}, {"not NAME=value", "K=v\nnot a line\n", 0o600}} {
		write(t, path, bad.text)
		if err := os.Chmod(path, bad.mode); err != nil {
			t.Fatal(err)
		}
		_, err = c.Secrets(ctx)
		refused(t, bad.what+", listed", err, api.CodeInternal, "")
		_, err = c.SetSecret(ctx, "NEW", "v", false)
		refused(t, bad.what+", set", err, api.CodeInternal, "")
		fileIs(t, path, bad.text, bad.mode)
	}

	// A symbolic link, a file with another hard link and a read-only one
	// would be replaced or cut; a FIFO would block.
	target := filepath.Join(h.root, "dotfiles.env")
	write(t, target, "K="+decoy+"\n")
	links := []struct {
		what string
		make func() error
		msg  string
	}{
		{"a symbolic link", func() error { return os.Symlink(target, path) }, "secrets.env is a symbolic link, which a write would replace: edit it by hand"},
		{"a hard link", func() error { return os.Link(target, path) }, "secrets.env has other hard links, which a write would leave as they are: edit it by hand"},
		{"read-only", func() error { write(t, path, "K="+decoy+"\n"); return os.Chmod(path, 0o400) }, "secrets.env is read-only: edit it by hand"},
		{"a FIFO", func() error { return syscall.Mkfifo(path, 0o600) }, "secrets.env is not a regular file: edit it by hand"},
	}
	for _, l := range links {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := l.make(); err != nil {
			t.Fatal(err)
		}
		_, err = c.SetSecret(ctx, "K", "v", true)
		refused(t, l.what+", set", err, api.CodeConflict, l.msg)
		_, err = c.UnsetSecret(ctx, "K", true)
		refused(t, l.what+", unset", err, api.CodeConflict, l.msg)
	}
	_, err = c.Secrets(ctx) // the FIFO
	refused(t, "a FIFO, listed", err, api.CodeInternal, "")
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO: %v %v", st, err)
	}
	fileIs(t, target, "K="+decoy+"\n", 0o600)

	// Through a symbolic link, the names are read.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if ss, err := c.Secrets(ctx); err != nil || fmt.Sprint(ss) != "[{K false}]" {
		t.Errorf("read through a link: %+v %v", ss, err)
	}
	if n := len(events(t, h, audit.SecretSet)) + len(events(t, h, audit.SecretUnset)); n != 0 {
		t.Errorf("%d audited", n)
	}
	noDecoy(t, h)
}

// Over TCP, every call on secrets.env needs config:write.
func TestSecretsScope(t *testing.T) {
	h := home(t, nil)
	write(t, h.p.SecretsFile(), "K="+decoy+"\n")
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	ctx := context.Background()
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	const msg = "this call needs the config:write scope (catchpen daemon client add --scope config:write)"
	for what, err := range map[string]error{
		"list":  second(laptop.Secrets(ctx)),
		"set":   second(laptop.SetSecret(ctx, "NEW", "v", true)),
		"unset": second(laptop.UnsetSecret(ctx, "K", true)),
	} {
		refused(t, what, err, api.CodeForbidden, msg)
	}
	fileIs(t, h.p.SecretsFile(), "K="+decoy+"\n", 0o600)

	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	if ss, err := cockpit.SetSecret(ctx, "K", "rotated", true); err != nil || fmt.Sprint(ss) != "[{K false}]" {
		t.Fatalf("with the scope: %+v %v", ss, err)
	}
	fileIs(t, h.p.SecretsFile(), "K=rotated\n", 0o600)
	if _, err := cockpit.UnsetSecret(ctx, "K", true); err != nil {
		t.Fatal(err)
	}
	// The events name the secret only to config:write clients (newest first).
	for _, tc := range []struct {
		c    *apiclient.Client
		want string
	}{{laptop, "[map[client:cockpit] map[client:cockpit]]"}, {cockpit, "[map[client:cockpit name:K] map[client:cockpit name:K replaced:true]]"}} {
		page, err := tc.c.Audit(ctx, apiclient.AuditQuery{})
		var got []map[string]any
		for _, e := range page.Events {
			got = append(got, e.Detail)
		}
		if err != nil || fmt.Sprint(got) != tc.want {
			t.Errorf("audit: %v %v, want %s", got, err, tc.want)
		}
	}
}

// A set waits for config.lock, and reads the file under it: a hand edit made
// meanwhile is kept. A bad request is refused without waiting.
func TestSecretsLock(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	unlock, err := fsutil.Lock(filepath.Join(h.p.Config, "config.lock"))
	if err != nil {
		t.Fatal(err)
	}
	for what, call := range map[string]func(context.Context) error{
		"a bad name":       func(ctx context.Context) error { return second(c.SetSecret(ctx, "9X", "v", true)) },
		"a bad value":      func(ctx context.Context) error { return second(c.SetSecret(ctx, "K", "", true)) },
		"a bad unset name": func(ctx context.Context) error { return second(c.UnsetSecret(ctx, "9X", true)) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := call(ctx); code(err) != api.CodeBadRequest {
			t.Errorf("%s, the lock held: %v", what, err)
		}
		cancel()
	}
	done := make(chan error, 1)
	go func() { done <- second(c.SetSecret(context.Background(), "NEW", "v", false)) }()
	select {
	case err := <-done:
		t.Fatalf("set without the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	write(t, h.p.SecretsFile(), "HAND=1\n")
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fileIs(t, h.p.SecretsFile(), "HAND=1\nNEW=v\n", 0o600)
}
