package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// At most maxRunning jobs run at once, and the last maxEnded that ended are
// kept for clients to read.
var (
	maxRunning = 16
	maxEnded   = 32
)

var (
	errBusy     = errors.New("too many jobs are running")
	errStopping = errors.New("the server is stopping")
)

// jobs runs the server's jobs, until stop.
type jobs struct {
	ctx     context.Context // a job's: done when the server stops
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	stopped bool
	all     []*job // oldest first
}

func newJobs() *jobs {
	js := &jobs{}
	js.ctx, js.cancel = context.WithCancel(context.Background())
	return js
}

// watch looks for a VM's decoys in a job's lines and error.
type watch struct {
	scan func(string) (*canary.Canary, error) // the decoy a text carries, if any
	hit  func(*canary.Canary)                 // acts on the first found, at once, in a goroutine of its own
}

// What clients get in place of a job's lines and error once a decoy is
// found in them, or the decoys can't be read.
const (
	withheldDecoy  = "(withheld: it contained the VM's decoy credentials)"
	withheldUnread = "(withheld: the VM's canaries couldn't be read)"
)

// What a client without config:write gets in place of a job's error or
// line that names a host secret.
const (
	withheldSecretError = "withheld: the job's error names a host secret: reading it needs the config:write scope"
	withheldSecretLine  = "withheld: the line names host secrets: reading it needs the config:write scope"
)

// job is one job, and the writer its progress lines are written to.
type job struct {
	mu       sync.Mutex
	info     api.JobInfo
	lines    []string             // the last api.MaxJobLines
	named    []bool               // for each of lines: it names host secrets
	part     []byte               // the start of a line not ended yet
	marked   bool                 // part holds text written as naming host secrets
	watch    *watch               // nil: not watched
	act      func(*canary.Canary) // runs watch.hit, without waiting
	hit      *canary.Canary
	withheld string // what clients get in place of every line and the error ("": the text)
	secret   bool   // the error names a host secret (a *vm.SecretError)
}

// Write takes progress output, a line at a time.
func (j *job) Write(b []byte) (int, error) { return j.write(b, false) }

// WriteSecret takes progress output that names host secrets (vm.SecretLines):
// every line it writes to is marked as naming them.
func (j *job) WriteSecret(b []byte) (int, error) { return j.write(b, true) }

func (j *job) write(b []byte, named bool) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(b)
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		j.add(append(j.part, b[:i]...), named || j.marked)
		j.part, j.marked, b = nil, false, b[i+1:]
	}
	if len(b) > 0 && named {
		j.marked = true
	}
	if len(j.part)+len(b) <= api.MaxJobLine { // what a line keeps, at most
		j.part = append(j.part, b...)
	} else {
		j.part = append(j.part, b[:api.MaxJobLine-len(j.part)]...)
	}
	return n, nil
}

// add keeps a line; named: it was written as naming host secrets.
func (j *job) add(line []byte, named bool) {
	s := string(line[:min(len(line), api.MaxJobLine)])
	prev := ""
	if len(j.lines) > 0 {
		prev = j.lines[len(j.lines)-1]
	}
	j.check(prev + s) // a decoy split across two lines too
	j.lines = append(j.lines, s)
	j.named = append(j.named, named || strings.HasPrefix(s, vm.MissingSecrets))
	if len(j.lines) > api.MaxJobLines {
		j.lines = slices.Delete(j.lines, 0, len(j.lines)-api.MaxJobLines)
		j.named = slices.Delete(j.named, 0, len(j.named)-api.MaxJobLines)
	}
	j.info.Written++
}

// check looks for a decoy in s; one found, or decoys that can't be read,
// withhold every line and the error from clients. The first found is acted
// on without waiting: whoever wrote s may hold the VM's lock, which acting
// can take. j.mu is held.
func (j *job) check(s string) {
	if j.watch == nil || j.hit != nil {
		return
	}
	c, err := j.watch.scan(s)
	switch {
	case c != nil:
		j.hit, j.withheld = c, withheldDecoy
		j.act(c)
	case err != nil && j.withheld == "":
		j.withheld = withheldUnread
	}
}

// end records how the job ended, its last line written out first.
func (j *job) end(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.part) > 0 {
		j.add(j.part, j.marked)
		j.part, j.marked = nil, false
	}
	j.info.State, j.info.Ended = api.JobDone, time.Now().UTC()
	if err != nil {
		last := ""
		if len(j.lines) > 0 {
			last = j.lines[len(j.lines)-1]
		}
		j.check(last + err.Error()) // a decoy split from the last line too
		j.info.State, j.info.Error = api.JobFailed, err.Error()
		var se *vm.SecretError
		j.secret = errors.As(err, &se)
	}
}

// get is the job's info as a config:write client gets it: for the server's
// own use, and the answer that starts the job (no error yet).
func (j *job) get() api.JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.shown(true)
}

// shown is the job's info as a client gets it (secrets: one with
// config:write); j.mu is held. A decoy's note wins.
func (j *job) shown(secrets bool) api.JobInfo {
	info := j.info
	switch {
	case j.withheld != "" && info.Error != "":
		info.Error = j.withheld
	case j.secret && !secrets:
		info.Error = withheldSecretError
	}
	return info
}

// from is the job with its lines from the after-th on (those kept), as a
// client gets it (secrets: one with config:write). A decoy's note wins.
func (j *job) from(after int, secrets bool) api.Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	first := j.info.Written - len(j.lines) // the number of the first line kept
	from := min(max(after, first), j.info.Written)
	lines := append([]string{}, j.lines[from-first:]...)
	for i := range lines {
		switch {
		case j.withheld != "":
			lines[i] = j.withheld
		case j.named[from-first+i] && !secrets:
			lines[i] = withheldSecretLine
		}
	}
	return api.Job{JobInfo: j.shown(secrets), From: from, Lines: lines}
}

// start runs fn as a job of kind on VM vm for client, with progress written
// to out. fn's ctx is done when the server stops, not when the client goes.
// w (nil: none) looks for the VM's decoys in the job's lines and error.
func (js *jobs) start(kind, vm, client string, w *watch, fn func(ctx context.Context, out io.Writer) error) (api.JobInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.stopped {
		return api.JobInfo{}, errStopping
	}
	running := 0
	for _, j := range js.all {
		if j.get().State == api.JobRunning {
			running++
		}
	}
	if running >= maxRunning {
		return api.JobInfo{}, errBusy
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	j := &job{info: api.JobInfo{ID: hex.EncodeToString(id), Kind: kind, VM: vm, Client: client,
		State: api.JobRunning, Created: time.Now().UTC()}, watch: w}
	if w != nil {
		j.act = func(c *canary.Canary) { js.wg.Go(func() { w.hit(c) }) } // stop waits for it too
	}
	js.all = append(js.all, j)
	js.wg.Go(func() {
		err := run(js.ctx, j, fn)
		js.mu.Lock() // once a job is seen ended, the jobs past maxEnded are gone
		defer js.mu.Unlock()
		j.end(err)
		js.prune()
	})
	return j.get(), nil
}

// prune drops the oldest of the jobs that ended, past maxEnded; js.mu is
// held.
func (js *jobs) prune() {
	ended := 0
	for i := len(js.all) - 1; i >= 0; i-- {
		if js.all[i].get().State != api.JobRunning {
			if ended++; ended > maxEnded {
				js.all = slices.Delete(js.all, i, i+1)
			}
		}
	}
}

// list is every job kept, newest first, as a client gets them (secrets:
// one with config:write).
func (js *jobs) list(secrets bool) []api.JobInfo {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]api.JobInfo, 0, len(js.all))
	for i := len(js.all) - 1; i >= 0; i-- {
		j := js.all[i]
		j.mu.Lock()
		out = append(out, j.shown(secrets))
		j.mu.Unlock()
	}
	return out
}

func (js *jobs) find(id string) (*job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.all {
		if j.info.ID == id { // set once, before it is shared
			return j, true
		}
	}
	return nil, false
}

// stop refuses new jobs, cancels those running and waits for them to end.
func (js *jobs) stop() {
	js.mu.Lock()
	js.stopped = true
	js.mu.Unlock()
	js.cancel()
	js.wg.Wait()
}

// run is fn, with a panic its error: the daemon and its other jobs run on.
func run(ctx context.Context, j *job, fn func(context.Context, io.Writer) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the job failed: panic: %v", p)
		}
	}()
	return fn(ctx, j)
}
