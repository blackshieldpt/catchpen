package task

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// File is a batch of tasks (curral task run --file).
type File struct {
	Defaults Spec   `yaml:"defaults"`
	Tasks    []Spec `yaml:"tasks"`
	// Matrix runs each task once per agent and repo (unless the task sets its own).
	Matrix struct {
		Agent []string `yaml:"agent"`
		Repo  []string `yaml:"repo"`
	} `yaml:"matrix"`
}

const maxBatch = 100

// ParseFile reads and expands a batch file; relative paths are resolved
// against the file's directory, and into defaults to cwd.
func ParseFile(path, cwd string) ([]Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Tasks) == 0 {
		return nil, fmt.Errorf("%s: no tasks", path)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	var out []Spec
	for i, t := range f.Tasks {
		own := t
		t = withDefaults(t, f.Defaults)
		agents, repos := []string{t.Agent}, []string{t.Repo}
		if own.Agent == "" && len(f.Matrix.Agent) > 0 {
			agents = f.Matrix.Agent
		}
		if own.Repo == "" && len(f.Matrix.Repo) > 0 {
			repos = f.Matrix.Repo
		}
		for _, a := range agents {
			for _, r := range repos {
				s := t
				s.Agent, s.Repo = a, r
				if err := s.Normalize(dir, cwd); err != nil {
					return nil, fmt.Errorf("%s: task %d (%s, %s): %w", path, i+1, a, r, err)
				}
				if out = append(out, s); len(out) > maxBatch {
					return nil, errors.New("more than 100 tasks in one batch")
				}
			}
		}
	}
	return out, nil
}

func withDefaults(t, d Spec) Spec {
	if t.Template == "" {
		t.Template = d.Template
	}
	if t.Repo == "" {
		t.Repo = d.Repo
	}
	if t.Agent == "" {
		t.Agent = d.Agent
	}
	if t.Cmd == "" {
		t.Cmd = d.Cmd
	}
	if t.Prompt == "" && t.PromptFile == "" {
		t.Prompt, t.PromptFile = d.Prompt, d.PromptFile
	}
	if t.Timeout == 0 {
		t.Timeout = d.Timeout
	}
	if t.MaxBudgetUSD == 0 {
		t.MaxBudgetUSD = d.MaxBudgetUSD
	}
	if t.Out == "" {
		t.Out = d.Out
	}
	if t.Into == "" {
		t.Into = d.Into
	}
	if t.Title == "" {
		t.Title = d.Title
	}
	t.RW = t.RW || d.RW
	t.Keep = t.Keep || d.Keep
	return t
}
