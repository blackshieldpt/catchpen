package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

var (
	dev  = runningVM{Name: "dev", Default: "claude", Agents: []string{"claude", "pi"}}
	web  = runningVM{Name: "web", Default: "claude", Agents: []string{"claude"}}
	bare = runningVM{Name: "bare"}
	job  = runningVM{Name: "job", Task: true, Default: "claude", Agents: []string{"claude"}}
	vms  = []runningVM{dev, web, bare, job}
)

func TestTmuxTargets(t *testing.T) {
	for _, c := range []struct {
		name      string
		vms       []runningVM
		args      []string
		all, sync bool
		names     []string
		err       string
	}{
		{name: "default: every VM but task VMs", vms: vms, names: []string{"dev", "web", "bare"}},
		{name: "named", vms: vms, args: []string{"web", "dev"}, names: []string{"web", "dev"}},
		{name: "--all ignores names", vms: vms, args: []string{"nope"}, all: true, names: []string{"dev", "web", "bare"}},
		{name: "--sync leaves task VMs out", vms: vms, sync: true, names: []string{"dev", "web", "bare"}},
		{name: "headless task VM named", vms: vms, args: []string{"job"}, err: "job runs a headless task"},
		{name: "not running", vms: vms, args: []string{"nope"}, err: "nope is not running"},
		{name: "--sync with a name", vms: vms, args: []string{"dev"}, sync: true, err: "--sync takes no VM names"},
		{name: "--sync --all", vms: vms, all: true, sync: true, err: "--sync takes no VM names"},
		{name: "only task VMs running", vms: []runningVM{job}},
	} {
		names, err := tmuxTargets(c.vms, c.args, c.all, c.sync)
		if c.err != "" {
			if err == nil || !strings.HasPrefix(err.Error(), c.err) || names != nil {
				t.Errorf("%s: %q, %v, want error %q", c.name, names, err, c.err)
			}
		} else if err != nil || !slices.Equal(names, c.names) {
			t.Errorf("%s: %q, %v, want %q", c.name, names, err, c.names)
		}
	}
	if err := noVMs([]runningVM{job}); err == nil || !strings.HasPrefix(err.Error(), "only headless task VMs are running") {
		t.Errorf("noVMs(task VMs) = %v", err)
	}
	if err := noVMs(nil); err == nil || err.Error() != "no running VMs" {
		t.Errorf("noVMs(nil) = %v", err)
	}
}

func TestHerdrTargets(t *testing.T) {
	for _, c := range []struct {
		name       string
		vms        []runningVM
		args       []string
		all, sync  bool
		agent      string
		names      []string
		agentOf    map[string]string
		notes, err string
	}{
		{name: "default: every VM but task VMs", vms: vms, names: []string{"dev", "web", "bare"},
			agentOf: map[string]string{"dev": "claude", "web": "claude", "bare": ""}},
		{name: "--all ignores names", vms: vms, args: []string{"nope"}, all: true, names: []string{"dev", "web", "bare"},
			agentOf: map[string]string{"dev": "claude", "web": "claude", "bare": ""}},
		{name: "--agent where installed, a note elsewhere", vms: vms, agent: "pi", names: []string{"dev", "web", "bare"},
			agentOf: map[string]string{"dev": "pi", "web": "claude", "bare": ""},
			notes:   "web has no pi; if it gets a new workspace, that runs claude|bare has no pi; if it gets a new workspace, that runs a shell"},
		{name: "--agent missing in a named VM", vms: vms, args: []string{"web"}, agent: "pi", err: "pi is not installed in web"},
		{name: "--all --agent, named VM without it", vms: vms, args: []string{"web"}, all: true, agent: "pi", err: "pi is not installed in web"},
		{name: "headless task VM named", vms: vms, args: []string{"job"}, err: "job runs a headless task"},
		{name: "not running", vms: vms, args: []string{"nope"}, err: "nope is not running"},
		{name: "only task VMs running", vms: []runningVM{job}, err: "only headless task VMs are running"},
		{name: "nothing running", err: "no running VMs"},
		{name: "--sync with a name", vms: vms, args: []string{"dev"}, sync: true, err: "--sync takes no VM names or --agent"},
		{name: "--sync with --agent", vms: vms, sync: true, agent: "pi", err: "--sync takes no VM names or --agent"},
		{name: "--sync --all", vms: vms, all: true, sync: true, err: "--sync takes no VM names or --agent"},
		{name: "--sync with nothing running", sync: true, agentOf: map[string]string{}},
		{name: "--sync", vms: vms, sync: true, names: []string{"dev", "web", "bare"},
			agentOf: map[string]string{"dev": "claude", "web": "claude", "bare": ""}},
	} {
		names, agentOf, notes, err := herdrTargets(c.vms, c.args, c.all, c.sync, c.agent)
		if c.err != "" {
			if err == nil || !strings.HasPrefix(err.Error(), c.err) || names != nil {
				t.Errorf("%s: %q, %v, want error %q", c.name, names, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !slices.Equal(names, c.names) || !maps.Equal(agentOf, c.agentOf) || strings.Join(notes, "|") != c.notes {
			t.Errorf("%s: names %q, agents %v, notes %q", c.name, names, agentOf, notes)
		}
	}
}
