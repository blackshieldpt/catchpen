package template

import (
	"fmt"
	"strings"
)

// SSH agent forwarding modes.
const (
	AgentOff        = "off"
	AgentRestricted = "restricted" // per-VM agent with only agent_keys, destination-constrained to the VM's forges
	AgentHost       = "host"       // forward the host's own agent (explicit opt-in)
)

type SSH struct {
	Agent     string   `yaml:"agent,omitempty" json:"agent,omitempty"`
	AgentKeys []string `yaml:"agent_keys,omitempty" json:"agent_keys,omitempty"` // host key files (~/ allowed)
	Confirm   *bool    `yaml:"confirm,omitempty" json:"confirm,omitempty"`       // ask (ssh-askpass) on each use; default true
}

func (s SSH) Mode() string {
	if s.Agent == "" {
		return AgentOff
	}
	return s.Agent
}

func (s SSH) ConfirmEach() bool { return s.Confirm == nil || *s.Confirm }

func (s SSH) Validate() error {
	switch s.Mode() {
	case AgentOff, AgentHost:
		if len(s.AgentKeys) > 0 && s.Mode() == AgentHost {
			return fmt.Errorf("ssh.agent_keys only applies to ssh.agent: restricted")
		}
	case AgentRestricted:
		if len(s.AgentKeys) == 0 {
			return fmt.Errorf("ssh.agent: restricted needs ssh.agent_keys")
		}
	default:
		return fmt.Errorf("ssh.agent must be off, restricted or host")
	}
	for _, k := range s.AgentKeys {
		if !(strings.HasPrefix(k, "/") || strings.HasPrefix(k, "~/")) || strings.ContainsAny(k, "\n\r\x00") {
			return fmt.Errorf("ssh.agent_keys: %q must be an absolute or ~/ path", k)
		}
	}
	return nil
}
