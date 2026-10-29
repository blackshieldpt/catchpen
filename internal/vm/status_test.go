package vm

import "testing"

// Statuses must describe each VM as catchpen ls always has: paused wins over
// running, and only VMs built from template picks can have a stale base.
func TestStatuses(t *testing.T) {
	m := testManager(t)
	cur, err := m.CurrentBase("minimal")
	if err != nil || cur == "" {
		t.Fatalf("CurrentBase(minimal) = %q, %v", cur, err)
	}
	vms := []Info{
		{State: State{Name: "stopped1", Template: "minimal"}},
		{State: State{Name: "run1", Template: "minimal"}, Running: true},
		{State: State{Name: "paused1", Template: "minimal"}, Running: true, Paused: true},
		{State: State{Name: "eph1", Template: "minimal", Ephemeral: true, Task: "0123abcd"}, Running: true},
		{State: State{Name: "stale1", Template: "minimal", Picks: true, Base: "/old/base.qcow2"}},
		{State: State{Name: "fresh1", Template: "minimal", Picks: true, Base: cur}},
		{State: State{Name: "nopicks1", Template: "minimal", Base: "/old/base.qcow2"}},
		{State: State{Name: "gone1", Template: "no-such-template", Picks: true, Base: "/old/base.qcow2"}},
	}
	want := map[string]string{
		"stopped1": "stopped",
		"run1":     "running",
		"paused1":  "paused",
		"eph1":     "running,ephemeral,task",
		"stale1":   "stopped,stale-base",
		"fresh1":   "stopped",
		"nopicks1": "stopped",
		"gone1":    "stopped", // a template that no longer resolves has no current base
	}
	got := m.Statuses(vms)
	if len(got) != len(want) {
		t.Errorf("got %d statuses, want %d", len(got), len(want))
	}
	for name, w := range want {
		if s := got[name].String(); s != w {
			t.Errorf("%s: %q, want %q", name, s, w)
		}
	}
	if s := (Status{State: "running", Ephemeral: true, Task: true, StaleBase: true}).String(); s != "running,ephemeral,task,stale-base" {
		t.Errorf("String order: %q", s)
	}
}
