package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/server"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

func TestUnit(t *testing.T) {
	env := map[string]string{"CATCHPEN_HOME": "/srv/cur ral%$x", "XDG_DATA_HOME": "relative", "PATH": "/usr/bin:/bin"}
	u := Unit("/opt/cur$ral/bin/catchpen", func(k string) string { return env[k] })
	for _, want := range []string{
		"Environment=\"CATCHPEN_HOME=/srv/cur ral%%$x\"\n",
		"Environment=\"PATH=/usr/bin:/bin\"\n",
		"ExecStart=\"/opt/cur$$ral/bin/catchpen\" daemon\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "XDG_DATA_HOME") {
		t.Error("relative XDG_DATA_HOME carried over")
	}
}

func testManager(t *testing.T) *vm.Manager {
	root := t.TempDir()
	p := paths.Paths{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data"), Cache: filepath.Join(root, "cache")}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &vm.Manager{P: p, Cfg: config.Default()}
}

func TestRebuildSchedule(t *testing.T) {
	m := testManager(t)
	d := &Daemon{Log: os.Stderr}
	ctx := context.Background()
	if err := d.rebuild(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stampPath(m.P)); err == nil {
		t.Error("rebuild: off wrote a stamp")
	}
	m.Cfg.Rebuild = "daily"
	if err := d.rebuild(ctx, m); err != nil { // first check: just the stamp
		t.Fatal(err)
	}
	b, err := os.ReadFile(stampPath(m.P))
	if err != nil {
		t.Fatal(err)
	}
	// Not due yet: the stamp stays.
	if err := d.rebuild(ctx, m); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(stampPath(m.P)); string(b2) != string(b) {
		t.Error("stamp rewritten before the interval")
	}
	// Due: no VMs, nothing to build, but it runs and stamps.
	if err := writeStamp(m.P, time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.rebuild(ctx, m); err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(stampPath(m.P))
	if last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b3))); err != nil || time.Since(last) > time.Minute {
		t.Errorf("stamp after a run = %q", b3)
	}
}

func TestPromptVMs(t *testing.T) {
	m := testManager(t)
	for name, onDeny := range map[string]string{"asks": egress.OnDenyPrompt, "logs": egress.OnDenyLog, "other": egress.OnDenyPrompt, "pool-1": egress.OnDenyPrompt} {
		dir := m.Dir(name)
		state := `{"name":"` + name + `","egress":"allowlist","pool":` + strconv.FormatBool(name == "pool-1") + `}`
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "vm.json"), []byte(state), 0o600)
		if err := egress.WriteConfig(dir, egress.Config{OnDeny: onDeny}); err != nil {
			t.Fatal(err)
		}
	}
	// Another watcher serves "other".
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { egress.Watch(ctx, map[string]string{"other": m.Dir("other")}, nil); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for !served(filepath.Join(m.Dir("other"), egress.SocketFile)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got, stale := promptVMs(m, nil)
	if len(got) != 1 || got["asks"] != m.Dir("asks") || stale {
		t.Errorf("promptVMs = %v %v", got, stale)
	}
	// What we serve ourselves stays ours; "asks" has no socket of ours (a VM
	// destroyed and created again), so the watcher must restart.
	ours := map[string]string{"asks": m.Dir("asks"), "other": m.Dir("other")}
	if got, stale := promptVMs(m, ours); len(got) != 2 || !stale {
		t.Errorf("promptVMs keeping ours = %v %v", got, stale)
	}
}

// syncLog is a log the daemon's goroutines can share.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// apiDaemon is a daemon on m's directories, with server: set when serve is.
func apiDaemon(m *vm.Manager, serve bool) (*Daemon, *syncLog) {
	cfg := m.Cfg
	if serve {
		cfg.Server = &config.Server{}
	}
	log := &syncLog{}
	return &Daemon{
		Manager: func() (*vm.Manager, error) { return &vm.Manager{P: m.P, Cfg: cfg}, nil },
		API:     server.New("1.2.3").Handler(),
		Log:     log,
	}, log
}

func TestRunServesAPI(t *testing.T) {
	m := testManager(t)
	d, log := apiDaemon(m, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	c, err := apiclient.New(apiclient.Options{Socket: m.P.APISocket()})
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !strings.Contains(log.String(), "catchpen daemon running"); time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("daemon not running: %s", log)
		}
	}
	if i, err := c.Info(ctx); err != nil || i.Version != "1.2.3" {
		t.Errorf("info %+v, %v", i, err)
	}
	// A second daemon fails, and leaves the first one's socket alone.
	d2, _ := apiDaemon(m, true)
	if err := d2.Run(ctx); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second daemon: %v", err)
	}
	if _, err := c.Info(ctx); err != nil {
		t.Errorf("after a second daemon: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon kept running after its context was done")
	}
	if _, err := os.Lstat(m.P.APISocket()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket after the daemon stopped: %v", err)
	}
}

// Without server:, the daemon leaves the socket's path alone; with it, a path
// it can't listen on stops the daemon.
func TestRunAPISetting(t *testing.T) {
	m := testManager(t)
	if err := os.Mkdir(m.P.APISocket(), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, log := apiDaemon(m, false)
	if err := d.Run(ctx); err != nil || strings.Contains(log.String(), "api:") {
		t.Errorf("without server: %v; log %q", err, log)
	}
	if fi, err := os.Stat(m.P.APISocket()); err != nil || !fi.IsDir() {
		t.Errorf("without server, the path was touched: %v", err)
	}
	d, _ = apiDaemon(m, true)
	if err := d.Run(ctx); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("with server: %v", err)
	}
}
