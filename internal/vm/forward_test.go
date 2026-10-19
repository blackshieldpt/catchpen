package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/template"
)

func TestForwardSpecs(t *testing.T) {
	for in, want := range map[string]string{"3000": "127.0.0.1:3000:localhost:3000", "8080:80": "127.0.0.1:8080:localhost:80", "9000:db:5432": "127.0.0.1:9000:db:5432"} {
		if got, err := LocalSpec(in); err != nil || got != want {
			t.Errorf("LocalSpec(%q) = %q, %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"5432": "127.0.0.1:5432:localhost:5432", "15432:5432": "127.0.0.1:15432:localhost:5432", "6379:cache.lan:6379": "127.0.0.1:6379:cache.lan:6379"} {
		if got, err := RemoteSpec(in); err != nil || got != want {
			t.Errorf("RemoteSpec(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "70000", "a:b", "0.0.0.0:80:x:80", "1:-oProxyCommand=x:2", "1:a b:2", "x"} {
		if _, err := LocalSpec(bad); err == nil {
			t.Errorf("LocalSpec(%q) accepted", bad)
		}
		if _, err := RemoteSpec(bad); err == nil {
			t.Errorf("RemoteSpec(%q) accepted", bad)
		}
	}
	if got, err := DynamicSpec("1080"); err != nil || got != "127.0.0.1:1080" {
		t.Errorf("DynamicSpec = %q, %v", got, err)
	}
	if _, err := DynamicSpec("1080:x"); err == nil {
		t.Error("bad -D accepted")
	}
}

func TestForwardNeedsRunningVM(t *testing.T) {
	m := testManager(t)
	writeFile(t, m.file("a", stateFile), `{"name":"a"}`)
	if _, err := m.Forward(context.Background(), "a", []string{"3000"}, nil, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}
	if fs, err := m.Forwards("a"); err != nil || len(fs) != 0 {
		t.Errorf("Forwards = %v, %v", fs, err)
	}
}

func TestCheckAgent(t *testing.T) {
	agentForge := map[string]forge.Forge{"g": {Name: "g", Auth: forge.Auth{Mode: forge.ModeAgent}}}
	tokenForge := map[string]forge.Forge{"t": {Name: "t", Auth: forge.Auth{Mode: forge.ModeToken}}}
	restricted := template.SSH{Agent: template.AgentRestricted, AgentKeys: []string{"~/.ssh/id"}}
	cases := []struct {
		ssh  template.SSH
		fs   map[string]forge.Forge
		want string
	}{
		{template.SSH{}, agentForge, "set ssh.agent"},
		{restricted, tokenForge, "needs a repo on a forge with auth mode agent"},
		{restricted, nil, "needs a repo"},
		{restricted, agentForge, ""},
		{template.SSH{Agent: template.AgentHost}, agentForge, "set ssh.agent: restricted"},
		{template.SSH{Agent: template.AgentHost}, tokenForge, ""},
		{template.SSH{}, tokenForge, ""},
	}
	for i, c := range cases {
		err := checkAgent(c.ssh, c.fs)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("case %d: err = %v, want %q", i, err, c.want)
		}
	}
}
