// Package task holds headless task specs, their on-disk records and the
// parsing of agent output. Running them is internal/vm's job.
package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// Agents a task can run; "cmd" runs Spec.Cmd with the prompt on stdin.
var Agents = []string{"claude", "opencode", "pi", "omp", "cmd"}

// Output modes.
const (
	OutPR     = "pr"
	OutBundle = "bundle" // fetch into refs/curral/<vm>/<branch> of the host repo
	OutPatch  = "patch"  // format-patch mbox in the task directory
	OutNone   = "none"
)

// MaxPrompt bounds a prompt (it is passed as one argument in the guest).
const MaxPrompt = 100 << 10

// Spec is what to run.
type Spec struct {
	Template     string        `json:"template,omitempty" yaml:"template"`
	Repo         string        `json:"repo,omitempty" yaml:"repo"`
	Agent        string        `json:"agent" yaml:"agent"`
	Cmd          string        `json:"cmd,omitempty" yaml:"cmd"`
	Prompt       string        `json:"-" yaml:"prompt"` // stored in prompt.md
	PromptFile   string        `json:"-" yaml:"prompt_file"`
	Timeout      time.Duration `json:"timeout" yaml:"timeout"`
	MaxBudgetUSD float64       `json:"max_budget_usd,omitempty" yaml:"max_budget_usd"`
	Out          string        `json:"out" yaml:"out"`
	Into         string        `json:"into,omitempty" yaml:"into"` // host repo for pr/bundle
	Title        string        `json:"title,omitempty" yaml:"title"`
	RW           bool          `json:"rw,omitempty" yaml:"rw"`
	Keep         bool          `json:"keep,omitempty" yaml:"keep"`
	Batch        string        `json:"batch,omitempty" yaml:"-"`
	Parallel     int           `json:"parallel,omitempty" yaml:"-"` // batch-wide limit
}

// Normalize fills defaults and validates. dir resolves relative paths;
// cwd is the default --into.
func (s *Spec) Normalize(dir, cwd string) error {
	if s.Agent == "" {
		s.Agent = "claude"
	}
	if !slices.Contains(Agents, s.Agent) {
		return fmt.Errorf("unknown agent %q (%s)", s.Agent, strings.Join(Agents, ", "))
	}
	if (s.Agent == "cmd") != (s.Cmd != "") {
		return errors.New("--cmd goes with --agent cmd (and only with it)")
	}
	if s.PromptFile != "" {
		if s.Prompt != "" {
			return errors.New("use a prompt or a prompt file, not both")
		}
		p := s.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s.Prompt, s.PromptFile = string(b), ""
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return errors.New("a task needs a prompt")
	}
	if len(s.Prompt) > MaxPrompt || strings.ContainsRune(s.Prompt, 0) {
		return fmt.Errorf("prompt too long (max %d KiB) or contains NUL", MaxPrompt>>10)
	}
	if s.Timeout == 0 {
		s.Timeout = 2 * time.Hour
	}
	if s.Timeout < time.Minute || s.Timeout > 48*time.Hour {
		return errors.New("timeout must be between 1m and 48h")
	}
	if s.MaxBudgetUSD < 0 || (s.MaxBudgetUSD > 0 && s.Agent != "claude") {
		return errors.New("max budget applies to claude only")
	}
	if s.Out == "" {
		s.Out = OutNone
		if s.Repo != "" {
			s.Out = OutPR
		}
	}
	switch s.Out {
	case OutPR, OutBundle, OutPatch:
		if s.Repo == "" {
			return fmt.Errorf("--out %s needs a --repo", s.Out)
		}
	case OutNone:
	default:
		return fmt.Errorf("--out must be pr, bundle, patch or none")
	}
	if s.Out == OutPR || s.Out == OutBundle {
		into := s.Into
		if into == "" {
			into = cwd
		}
		if !filepath.IsAbs(into) {
			into = filepath.Join(dir, into)
		}
		s.Into = filepath.Clean(into)
	} else {
		s.Into = ""
	}
	return nil
}

// States.
const (
	Queued    = "queued"
	Starting  = "starting" // holds a budget slot, VM being created
	Running   = "running"
	Exporting = "exporting"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Final reports whether a state is terminal.
func Final(state string) bool { return state == Done || state == Failed || state == Cancelled }

// Usage is what the agent reported spending.
type Usage struct {
	Turns        int     `json:"turns,omitempty"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Task is the record in tasks/<id>/task.json.
type Task struct {
	ID       string    `json:"id"`
	Spec     Spec      `json:"spec"`
	State    string    `json:"state"`
	VM       string    `json:"vm"`
	Created  time.Time `json:"created"`
	Started  time.Time `json:"started,omitzero"`
	Ended    time.Time `json:"ended,omitzero"`
	PID      int       `json:"pid,omitempty"` // supervisor
	MemoryMB int       `json:"memory_mb,omitempty"`
	Exit     *int      `json:"exit,omitempty"` // agent exit status
	Error    string    `json:"error,omitempty"`
	Usage    Usage     `json:"usage,omitzero"`
	Branch   string    `json:"branch,omitempty"`
	Commits  int       `json:"commits,omitempty"`
	Output   string    `json:"output,omitempty"` // PR URL, ref or patch file
}

// Store keeps tasks under Dir.
type Store struct{ Dir string }

var idRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// NewID returns a fresh task id.
func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ValidID checks an id from the command line.
func ValidID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid task id %q", id)
	}
	return nil
}

func (s Store) Path(id, file string) string { return filepath.Join(s.Dir, id, file) }

// Create writes a new task record and its prompt.
func (s Store) Create(t *Task, prompt string) error {
	if err := os.MkdirAll(filepath.Join(s.Dir, t.ID), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.Path(t.ID, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return err
	}
	return s.Save(t)
}

// Save writes a task record atomically.
func (s Store) Save(t *Task) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.Path(t.ID, "task.json"), append(b, '\n'), 0o600)
}

// Load reads a task record.
func (s Store) Load(id string) (*Task, error) {
	if err := ValidID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.Path(id, "task.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no task %s (see: curral task ls)", id)
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("task %s: %w", id, err)
	}
	return &t, nil
}

// Update loads, changes and saves a task under its lock.
func (s Store) Update(id string, fn func(*Task) error) (*Task, error) {
	unlock, err := fsutil.Lock(s.Path(id, "task.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	t, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if err := fn(t); err != nil {
		return t, err
	}
	return t, s.Save(t)
}

// List returns all tasks, newest first.
func (s Store) List() ([]*Task, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range ents {
		if !e.IsDir() || ValidID(e.Name()) != nil {
			continue
		}
		if t, err := s.Load(e.Name()); err == nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *Task) int { return b.Created.Compare(a.Created) })
	return out, nil
}

// Alive reports whether the task's supervisor process is running.
func (t *Task) Alive() bool {
	if t.PID <= 0 {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(t.PID) + "/cmdline")
	if err != nil {
		return false
	}
	args := strings.Split(string(b), "\x00")
	return slices.Contains(args, "task-supervise") && slices.Contains(args, t.ID)
}

// Status is the state to show: a non-final task whose supervisor died is lost.
func (t *Task) Status() string {
	if !Final(t.State) && t.PID > 0 && !t.Alive() {
		return "lost"
	}
	return t.State
}

// Signal sends sig to a live supervisor.
func (t *Task) Signal(sig syscall.Signal) error {
	if !t.Alive() {
		return fmt.Errorf("task %s is not running", t.ID)
	}
	return syscall.Kill(t.PID, sig)
}
