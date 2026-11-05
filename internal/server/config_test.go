package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/fsutil"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// fileIs fails t unless the file at path holds want, with mode.
func fileIs(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Errorf("%s: %q %v, want %q", filepath.Base(path), b, err, want)
		return
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
		t.Errorf("%s: mode %v %v, want %v", filepath.Base(path), st.Mode().Perm(), err, mode)
	}
}

// refused fails t unless err is code with a message starting msg.
func refused(t *testing.T, what string, err error, code, msg string) {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != code || !strings.HasPrefix(e.Message, msg) {
		t.Errorf("%s: %v, want %s: %s", what, err, code, msg)
	}
}

// loadingHome is home, its Manager loading config.yaml for each request, as
// the daemon's does.
func loadingHome(t *testing.T) *vmHost {
	t.Helper()
	var h *vmHost
	h = home(t, func() (*vm.Manager, error) {
		cfg, err := config.Load(h.p.ConfigFile())
		if err != nil {
			return nil, err
		}
		return &vm.Manager{P: h.p, Cfg: cfg, Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	return h
}

func TestConfigFile(t *testing.T) {
	h := loadingHome(t)
	c := h.client(t)
	ctx := context.Background()
	f, err := c.Config(ctx)
	if err != nil || f.Exists || f.Hash != "" || f.Text != "" || !strings.Contains(f.Effective, "max_vms: 2") {
		t.Fatalf("no file: %+v %v", f, err)
	}

	text := "tasks:\n  max_vms: 5\n"
	w, err := c.WriteConfig(ctx, text, "")
	if err != nil || !w.Exists || w.Text != text || w.Hash != hashOf([]byte(text)) || !strings.Contains(w.Effective, "max_vms: 5") {
		t.Fatalf("created: %+v %v", w, err)
	}
	fileIs(t, h.p.ConfigFile(), text, 0o600)
	if ev := lastEvent(t, h.p, audit.ConfigWrite); ev.Detail["file"] != "config.yaml" || ev.Detail["old_hash"] != "" ||
		ev.Detail["new_hash"] != w.Hash || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}
	if f, err := c.Config(ctx); err != nil || *f != *w {
		t.Errorf("read back: %+v %v", f, err)
	}

	// A write keeps the file's mode, and replaces only the file it was read from.
	if err := os.Chmod(h.p.ConfigFile(), 0o640); err != nil {
		t.Fatal(err)
	}
	text2 := "# by hand\ntasks:\n  max_vms: 3\n"
	w2, err := c.WriteConfig(ctx, text2, w.Hash)
	if err != nil || !strings.Contains(w2.Effective, "max_vms: 3") {
		t.Fatalf("rewritten: %+v %v", w2, err)
	}
	fileIs(t, h.p.ConfigFile(), text2, 0o640)
	if ev := lastEvent(t, h.p, audit.ConfigWrite); ev.Detail["old_hash"] != w.Hash || ev.Detail["new_hash"] != w2.Hash {
		t.Errorf("audit: %+v", ev)
	}
	_, err = c.WriteConfig(ctx, text, w.Hash)
	refused(t, "a stale hash", err, api.CodeConflict, "config.yaml changed since it was read: read it again")
	_, err = c.WriteConfig(ctx, text, "")
	refused(t, "no hash, a file", err, api.CodeConflict, "config.yaml changed since it was read")
	_, err = c.WriteConfig(ctx, "server: {}\n", w2.Hash)
	refused(t, "the API turned on", err, api.CodeConflict, "server: decides")
	fileIs(t, h.p.ConfigFile(), text2, 0o640)
	if n := len(events(t, h, audit.ConfigWrite)); n != 2 {
		t.Errorf("%d writes audited", n)
	}

	// The settings told are those of the text told, not of the file the
	// request's Manager loaded.
	write(t, h.p.ConfigFile(), "tasks:\n  max_vms: 7\n")
	if f, err := c.Config(ctx); err != nil || !strings.Contains(f.Effective, "max_vms: 7") {
		t.Errorf("hand edit: %+v %v", f, err)
	}

	// A config.yaml that doesn't load fails the calls, which need it, as every
	// other: it is fixed by hand.
	write(t, h.p.ConfigFile(), "tasks: [")
	if _, err := c.Config(ctx); code(err) != api.CodeInternal {
		t.Errorf("broken: %v", err)
	}
	_, err = c.WriteConfig(ctx, text, hashOf([]byte("tasks: [")))
	refused(t, "broken, written", err, api.CodeInternal, "")
	fileIs(t, h.p.ConfigFile(), "tasks: [", 0o640)
}

func TestConfigRefused(t *testing.T) {
	h := loadingHome(t)
	c := h.client(t)
	ctx := context.Background()
	const image = "images:\n  my-distro: {url: https://images.example.com/my.qcow2, sha256: " + "0000000000000000000000000000000000000000000000000000000000000000}\n"
	text := "server:\n  listen: 127.0.0.1:7443\nidle_stop: 1h\n" + image
	write(t, h.p.ConfigFile(), text)
	write(t, filepath.Join(h.p.Templates(), "distros.yaml"), "default: mine\nmine:\n  distro: my-distro\n")
	base := hashOf([]byte(text))

	for _, tc := range []struct{ what, text, code, msg string }{
		{"not YAML", "tasks: [", api.CodeBadRequest, h.p.ConfigFile() + ": yaml:"},
		{"an unknown field", "server:\n  listen: 127.0.0.1:7443\nnosuch: 1\n", api.CodeBadRequest, h.p.ConfigFile() + ": yaml: unmarshal errors:"},
		{"a bad value", "server:\n  listen: 127.0.0.1:7443\ncanary_action: explode\n", api.CodeBadRequest, h.p.ConfigFile() + ": canary_action must be pause, stop or log"},
		{"server: moved", "server:\n  listen: 0.0.0.0:7443\n", api.CodeConflict, "server: decides whether and where the API is served, so a write over the API can't change it: edit config.yaml by hand"},
		{"server: removed", "idle_stop: 1h\n", api.CodeConflict, "server: decides"},
		{"a host command, server: removed", "secrets:\n  X:\n    command: [touch, " + filepath.Join(h.root, "ran") + "]\n", api.CodeConflict, "server: decides"},
		{"a pool template that doesn't resolve", "server:\n  listen: 127.0.0.1:7443\npool:\n  - {template: nosuch, size: 1}\n" + image, api.CodeBadRequest, `pool template nosuch: unknown section "nosuch"`},
		{"an image the default uses dropped", "server:\n  listen: 127.0.0.1:7443\n", api.CodeBadRequest, `-t default: unknown distro "my-distro"`},
		{"an image that doesn't load", "server:\n  listen: 127.0.0.1:7443\nimages:\n  my-distro: {url: https://images.example.com/my.qcow2}\n", api.CodeBadRequest, "-t default: distro my-distro: set exactly one of checksum_url, sha256, sha512"},
	} {
		_, err := c.WriteConfig(ctx, tc.text, base)
		refused(t, tc.what, err, tc.code, tc.msg)
	}
	fileIs(t, h.p.ConfigFile(), text, 0o600)
	if _, err := os.Stat(filepath.Join(h.root, "ran")); err == nil {
		t.Error("a secret's command ran")
	}

	// server: as it is, other settings changed; a secret's command doesn't
	// run.
	ok := "server:\n  listen: 127.0.0.1:7443\nidle_stop: 2h\n" + image + "secrets:\n  X:\n    command: [touch, " + filepath.Join(h.root, "ran") + "]\n"
	if _, err := c.WriteConfig(ctx, ok, base); err != nil {
		t.Errorf("server: kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "ran")); err == nil {
		t.Error("a secret's command ran")
	}
	base = hashOf([]byte(ok))

	// A symbolic link would be replaced, not the file it names.
	target := filepath.Join(h.root, "dotfiles.yaml")
	write(t, target, ok)
	if err := os.Remove(h.p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, h.p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if f, err := c.Config(ctx); err != nil || f.Hash != base {
		t.Errorf("read through a link: %+v %v", f, err)
	}
	_, err := c.WriteConfig(ctx, ok, base)
	refused(t, "a link", err, api.CodeConflict, "config.yaml is a symbolic link, which a write would replace: edit it by hand")
	if st, err := os.Lstat(h.p.ConfigFile()); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link: %v %v", st, err)
	}

	// A hard link would be cut from the other names; a read-only file is
	// meant to stay as it is.
	if err := os.Remove(h.p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, h.p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	_, err = c.WriteConfig(ctx, ok, base)
	refused(t, "a hard link", err, api.CodeConflict, "config.yaml has other hard links, which a write would leave as they are: edit it by hand")
	if err := os.Remove(h.p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	write(t, h.p.ConfigFile(), ok)
	if err := os.Chmod(h.p.ConfigFile(), 0o400); err != nil {
		t.Fatal(err)
	}
	_, err = c.WriteConfig(ctx, ok, base)
	refused(t, "read-only", err, api.CodeConflict, "config.yaml is read-only: edit it by hand")
	fileIs(t, h.p.ConfigFile(), ok, 0o400)
	fileIs(t, target, ok, 0o600)

	code, body := h.post(t, api.PathConfig, `{"text":"","base_hash":"","mode":"0777"}`)
	if code != 400 || !strings.Contains(body, "invalid request body") {
		t.Errorf("an unknown field: %d %s", code, body)
	}
	if n := len(events(t, h, audit.ConfigWrite)); n != 1 {
		t.Errorf("%d writes audited", n)
	}
}

// userSections is a network.yaml with a section of its own.
const userSections = "default:\n  - allowlist\nlocal:\n  description: my registry\n  network:\n    allow: [registry.local]\n"

func TestTemplateFile(t *testing.T) {
	h := loadingHome(t)
	c := h.client(t)
	ctx := context.Background()
	path := filepath.Join(h.p.Templates(), "network.yaml")
	f, err := c.TemplateFile(ctx, "network")
	if err != nil || f.Exists || f.Hash != "" || f.Text != "" || f.Category != "network" || f.Builtin != template.BuiltinFile("network") || f.Builtin == "" {
		t.Fatalf("no file: %+v %v", f, err)
	}

	w, err := c.WriteTemplateFile(ctx, "network", userSections, "")
	if err != nil || !w.Exists || w.Text != userSections || w.Hash != hashOf([]byte(userSections)) || w.Builtin != f.Builtin {
		t.Fatalf("created: %+v %v", w, err)
	}
	fileIs(t, path, userSections, 0o600)
	if ev := lastEvent(t, h.p, audit.ConfigWrite); ev.Detail["file"] != "templates/network.yaml" || ev.Detail["old_hash"] != "" ||
		ev.Detail["new_hash"] != w.Hash || ev.Detail["client"] != api.ClientLocal {
		t.Errorf("audit: %+v", ev)
	}
	if f, err := c.TemplateFile(ctx, "network"); err != nil || *f != *w {
		t.Errorf("read back: %+v %v", f, err)
	}
	s, err := c.ShowTemplate(ctx, " local ")
	if err != nil || s.Spec != "local" || !strings.Contains(s.YAML, "registry.local") || !strings.HasPrefix(s.Base, h.p.Bases()) {
		t.Fatalf("show: %+v %v", s, err)
	}
	var net *api.TemplatePart
	for i, p := range s.Parts {
		if p.Category == "network" {
			net = &s.Parts[i]
		}
	}
	if net == nil || strings.Join(net.Sections, ",") != "allowlist,local" || net.Default || !s.Parts[0].Default {
		t.Errorf("parts: %+v", s.Parts)
	}

	// A pool entry's template must still resolve.
	pool, err := c.WriteConfig(ctx, "pool:\n  - {template: local, size: 1}\n", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteTemplateFile(ctx, "network", w.Hash, true)
	refused(t, "a pool's section", err, api.CodeBadRequest, `pool template local: unknown section "local"`)
	if _, err := c.WriteConfig(ctx, "", pool.Hash); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteTemplateFile(ctx, "network", w.Hash, false)
	refused(t, "no confirm", err, api.CodeConfirmRequired, `removing templates/network.yaml can't be undone (the built-in sections apply again): send "confirm": true`)
	_, err = c.DeleteTemplateFile(ctx, "network", "", true)
	refused(t, "a stale hash", err, api.CodeConflict, "templates/network.yaml changed since it was read: read it again")
	fileIs(t, path, userSections, 0o600)
	d, err := c.DeleteTemplateFile(ctx, "network", w.Hash, true)
	if err != nil || d.Exists || d.Text != "" || d.Hash != "" || d.Builtin != f.Builtin {
		t.Fatalf("deleted: %+v %v", d, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}
	if ev := lastEvent(t, h.p, audit.ConfigWrite); ev.Detail["file"] != "templates/network.yaml" || ev.Detail["old_hash"] != w.Hash || ev.Detail["new_hash"] != "" {
		t.Errorf("audit: %+v", ev)
	}
	_, err = c.DeleteTemplateFile(ctx, "network", w.Hash, true)
	refused(t, "deleted", err, api.CodeNotFound, "templates/network.yaml doesn't exist")

	// The fields, by their wire names.
	empty := hashOf(nil)
	if code, body := h.post(t, api.PathTemplateFile("idle"), `{"text":"","base_hash":""}`); code != 200 ||
		!strings.Contains(body, `"category":"idle","text":"","hash":"`+empty+`","exists":true,"builtin":"`) {
		t.Errorf("write on the wire: %d %s", code, body)
	}
	if code, body := h.post(t, api.PathTemplateFileDelete("idle"), `{"base_hash":"`+empty+`","confirm":true}`); code != 200 ||
		!strings.Contains(body, `"hash":"","exists":false`) {
		t.Errorf("delete on the wire: %d %s", code, body)
	}
	_, err = c.ShowTemplate(ctx, "local")
	refused(t, "show deleted", err, api.CodeBadRequest, `unknown section "local"`)
	if n := len(events(t, h, audit.ConfigWrite)); n != 6 {
		t.Errorf("%d writes audited", n)
	}
}

func TestTemplateFileRefused(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	ctx := context.Background()
	dir := h.p.Templates()
	write(t, filepath.Join(dir, "network.yaml"), userSections)
	write(t, filepath.Join(dir, "profiles.yaml"), "mine: [local]\n")
	base := hashOf([]byte(userSections))

	for _, tc := range []struct{ what, category, text, code, msg string }{
		{"not YAML", "network", "local: [", api.CodeBadRequest, filepath.Join(dir, "network.yaml") + ": yaml:"},
		{"a field of another category", "network", "x:\n  distro: debian-13\n", api.CodeBadRequest, filepath.Join(dir, "network.yaml") + ":2: x: distro doesn't belong in network.yaml"},
		{"a profile's section dropped", "network", "default:\n  - allowlist\n", api.CodeBadRequest, `-t profiles:mine: profiles.yaml mine: unknown section "local" (catchpen template ls lists them)`},
		{"a default preset that doesn't exist", "network", "default:\n  - allowlist\n  - badp\nlocal:\n  network:\n    allow: [registry.local]\nbadp:\n  network: {presets: [nosuch]}\n", api.CodeBadRequest, `-t default: unknown egress preset "nosuch"`},
		{"a default allow entry that doesn't parse", "network", "default:\n  - allowlist\n  - bada\nlocal:\n  network:\n    allow: [registry.local]\nbada:\n  network: {allow: [\"http://x\"]}\n", api.CodeBadRequest, "-t default: network.allow:"},
		{"a default distro that doesn't resolve", "distros", "default:\n  distro: nosuch-distro\n", api.CodeBadRequest, `-t default: unknown distro "nosuch-distro"`},
		{"no such category", "nosuch", "", api.CodeNotFound, "no such template category (distros, resources,"},
		{"a path", "..%2Fconfig", "", api.CodeNotFound, "no such template category"},
	} {
		b := base
		if tc.category == "distros" {
			b = ""
		}
		_, err := c.WriteTemplateFile(ctx, tc.category, tc.text, b)
		refused(t, tc.what, err, tc.code, tc.msg)
	}
	write(t, filepath.Join(dir, "resources.yaml"), "")
	_, err := c.WriteTemplateFile(ctx, "resources", "default:\n  resources: {memory: lots}\n", hashOf(nil))
	refused(t, "a default that doesn't load", err, api.CodeBadRequest, "-t default: resources need cpus, memory and disk")
	_, err = c.WriteTemplateFile(ctx, "network", userSections, "")
	refused(t, "created, but there", err, api.CodeConflict, "templates/network.yaml changed since it was read: read it again")
	_, err = c.DeleteTemplateFile(ctx, "network", base, true)
	refused(t, "deleted, a profile's section with it", err, api.CodeBadRequest, `-t profiles:mine: profiles.yaml mine: unknown section "local" (catchpen template ls lists them)`)
	_, err = c.TemplateFile(ctx, "..%2Fconfig")
	refused(t, "read a path", err, api.CodeNotFound, "no such template category")
	_, err = c.DeleteTemplateFile(ctx, "nosuch", "", true)
	refused(t, "delete nosuch", err, api.CodeNotFound, "no such template category")
	_, err = c.ShowTemplate(ctx, "nosuch")
	refused(t, "show nosuch", err, api.CodeBadRequest, `unknown section "nosuch"`)
	fileIs(t, filepath.Join(dir, "network.yaml"), userSections, 0o600)
	fileIs(t, filepath.Join(dir, "profiles.yaml"), "mine: [local]\n", 0o600)

	// A write may not break what worked; what didn't work may stay so, as
	// after a hand edit: a section, or a profile, may be meant only with
	// other picks.
	broken := userSections + "bad:\n  network: {egress: bogus}\n"
	if _, err := c.WriteTemplateFile(ctx, "network", broken, base); err != nil {
		t.Errorf("a section not picked: %v", err)
	}
	if _, err := c.WriteTemplateFile(ctx, "profiles", "mine: [local]\nyours: [bad]\n", hashOf([]byte("mine: [local]\n"))); err != nil {
		t.Errorf("a new profile that doesn't resolve: %v", err)
	}
	if _, err := c.WriteTemplateFile(ctx, "idle", "", ""); err != nil {
		t.Errorf("a write beside a profile that doesn't resolve: %v", err)
	}
	_, err = c.WriteTemplateFile(ctx, "network", "default:\n  - allowlist\nbad:\n  network: {egress: bogus}\n", hashOf([]byte(broken)))
	refused(t, "a working profile broken beside it", err, api.CodeBadRequest, "-t profiles:mine:")
	for _, f := range []string{"network.yaml", "profiles.yaml"} {
		text := map[string]string{"network.yaml": userSections, "profiles.yaml": "mine: [local]\n"}[f]
		if err := os.WriteFile(filepath.Join(dir, f), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file broken by hand doesn't stop a write to another.
	write(t, filepath.Join(dir, "caches.yaml"), "x: [")
	if _, err := c.WriteTemplateFile(ctx, "idle", "# x\n", hashOf(nil)); err != nil {
		t.Errorf("a write beside a file that doesn't load: %v", err)
	}
	for _, f := range []string{"idle.yaml", "caches.yaml"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}

	// Anything but a regular file is refused before it is read: a FIFO would
	// block the write, and the lock with it.
	if err := syscall.Mkfifo(filepath.Join(dir, "sync.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = c.WriteTemplateFile(ctx, "sync", "", "")
	refused(t, "a FIFO", err, api.CodeConflict, "templates/sync.yaml is not a regular file: edit it by hand")
	_, err = c.DeleteTemplateFile(ctx, "sync", "", true)
	refused(t, "delete a FIFO", err, api.CodeConflict, "templates/sync.yaml is not a regular file")
	_, err = c.TemplateFile(ctx, "sync")
	refused(t, "read a FIFO", err, api.CodeInternal, "")
	fileIs(t, filepath.Join(dir, "resources.yaml"), "", 0o600)
	if _, err := os.Stat(h.p.ConfigFile()); err == nil {
		t.Error("written outside templates/")
	}

	target := filepath.Join(h.root, "dotfiles.yaml")
	write(t, target, "")
	if err := os.Symlink(target, filepath.Join(dir, "ssh.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err = c.WriteTemplateFile(ctx, "ssh", "", hashOf(nil))
	refused(t, "a link", err, api.CodeConflict, "templates/ssh.yaml is a symbolic link, which a write would replace: edit it by hand")
	_, err = c.DeleteTemplateFile(ctx, "ssh", hashOf(nil), true)
	refused(t, "delete a link", err, api.CodeConflict, "templates/ssh.yaml is a symbolic link")
	if _, err := os.Lstat(filepath.Join(dir, "ssh.yaml")); err != nil {
		t.Errorf("the link: %v", err)
	}
	code, body := h.post(t, api.PathTemplateFile("network"), `{"text":"","base_hash":"","confirm":true}`)
	if code != 400 || !strings.Contains(body, "invalid request body") {
		t.Errorf("an unknown field: %d %s", code, body)
	}
	code, body = h.post(t, api.PathTemplateFileDelete("network"), `{"base_hash":"","text":""}`)
	if code != 400 || !strings.Contains(body, "invalid request body") {
		t.Errorf("an unknown delete field: %d %s", code, body)
	}
	if n := len(events(t, h, audit.ConfigWrite)); n != 4 {
		t.Errorf("%d writes audited", n)
	}
}

// Over TCP, every call on the configuration files needs config:write.
func TestConfigScope(t *testing.T) {
	h := home(t, nil)
	write(t, h.p.ConfigFile(), "# "+decoy+"\ntasks: {max_vms: 9}\n")
	write(t, filepath.Join(h.p.Templates(), "env.yaml"), "default:\n  env: {TOKEN: "+decoy+"}\n")
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	ctx := context.Background()
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	const msg = "this call needs the config:write scope (catchpen daemon client add --scope config:write)"
	for what, err := range map[string]error{
		"config":         second(laptop.Config(ctx)),
		"write config":   second(laptop.WriteConfig(ctx, "", hashOf([]byte("# "+decoy+"\ntasks: {max_vms: 9}\n")))),
		"template file":  second(laptop.TemplateFile(ctx, "env")),
		"write template": second(laptop.WriteTemplateFile(ctx, "network", "", "")),
		"delete":         second(laptop.DeleteTemplateFile(ctx, "env", "", true)),
		"show":           second(laptop.ShowTemplate(ctx, "")),
	} {
		refused(t, what, err, api.CodeForbidden, msg)
		if err != nil && strings.Contains(err.Error(), decoy) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.p.Templates(), "network.yaml")); err == nil {
		t.Error("written without the scope")
	}
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})
	// The settings told are the text's, not those of the Manager (the defaults).
	if c, err := cockpit.Config(ctx); err != nil || !strings.Contains(c.Effective, "max_vms: 9") {
		t.Errorf("config: %+v %v", c, err)
	}
	if err := os.Chmod(filepath.Join(h.p.Templates(), "env.yaml"), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := cockpit.TemplateFile(ctx, "env")
	if err != nil || !strings.Contains(f.Text, decoy) {
		t.Fatalf("with the scope: %+v %v", f, err)
	}
	if _, err := cockpit.WriteTemplateFile(ctx, "env", "", f.Hash); err != nil {
		t.Fatal(err)
	}
	fileIs(t, filepath.Join(h.p.Templates(), "env.yaml"), "", 0o640) // its mode kept
	if ev := lastEvent(t, h.p, audit.ConfigWrite); ev.Detail["client"] != "cockpit" || ev.Detail["file"] != "templates/env.yaml" {
		t.Errorf("audit: %+v", ev)
	}

	// A file's hashes confirm guesses of what it holds: only config:write
	// clients read them in the audit log.
	for _, tc := range []struct {
		c      *apiclient.Client
		hashes bool
	}{{laptop, false}, {cockpit, true}} {
		page, err := tc.c.Audit(ctx, apiclient.AuditQuery{})
		if err != nil || len(page.Events) != 1 || page.Events[0].Event != audit.ConfigWrite {
			t.Fatalf("audit: %+v %v", page, err)
		}
		d := page.Events[0].Detail
		_, old := d["old_hash"]
		_, new := d["new_hash"]
		if old != tc.hashes || new != tc.hashes || d["file"] != "templates/env.yaml" || d["client"] != "cockpit" {
			t.Errorf("hashes %v: %+v", tc.hashes, d)
		}
	}
}

// A write waits for its file's lock, and checks the hash under it: a write
// made meanwhile is not lost.
func TestConfigLock(t *testing.T) {
	h := home(t, nil)
	c := h.client(t)
	ctx := context.Background()
	for _, f := range []struct {
		name, path, lock string
		write            func(text, base string) error
	}{
		{"config.yaml", h.p.ConfigFile(), filepath.Join(h.p.Config, "config.lock"), func(text, base string) error { return second(c.WriteConfig(ctx, text, base)) }},
		{"templates/idle.yaml", filepath.Join(h.p.Templates(), "idle.yaml"), filepath.Join(h.p.Config, "config.lock"),
			func(text, base string) error { return second(c.WriteTemplateFile(ctx, "idle", text, base)) }},
	} {
		unlock, err := fsutil.Lock(f.lock)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- f.write("# mine\n", "") }()
		select {
		case err := <-done:
			t.Errorf("%s: written without the lock: %v", f.name, err)
			unlock()
			continue
		case <-time.After(300 * time.Millisecond):
		}
		write(t, f.path, "# theirs\n")
		unlock()
		refused(t, f.name, <-done, api.CodeConflict, f.name+" changed since it was read: read it again")
		fileIs(t, f.path, "# theirs\n", 0o600)
	}
}

// A template file is checked with config.yaml as it is under the lock, not
// as the request's Manager loaded it: a config write made meanwhile counts.
func TestTemplateFileLockedConfig(t *testing.T) {
	h := home(t, nil) // its Manager has the defaults: no pool
	c := h.client(t)
	ctx := context.Background()
	write(t, filepath.Join(h.p.Templates(), "network.yaml"), userSections)
	write(t, h.p.ConfigFile(), "pool:\n  - {template: local, size: 1}\n")
	_, err := c.DeleteTemplateFile(ctx, "network", hashOf([]byte(userSections)), true)
	refused(t, "a pool's section", err, api.CodeBadRequest, `pool template local: unknown section "local"`)
	_, err = c.WriteTemplateFile(ctx, "network", "", hashOf([]byte(userSections)))
	refused(t, "a pool's section dropped", err, api.CodeBadRequest, `pool template local: unknown section "local"`)
	write(t, h.p.ConfigFile(), "tasks: [")
	_, err = c.WriteTemplateFile(ctx, "network", userSections, hashOf([]byte(userSections)))
	refused(t, "a config.yaml that doesn't load", err, api.CodeInternal, "")
	fileIs(t, filepath.Join(h.p.Templates(), "network.yaml"), userSections, 0o600)
}
