package api

// PathEgressPrompts is GET, upgraded to a WebSocket: the egress prompts
// waiting for an answer, and the client's answers. With no eligible client
// connected (a task VM's prompt needs config:write),
// the daemon falls back to desktop notifications.
const PathEgressPrompts = Prefix + "/egress/prompts"

// Prompt message types.
const (
	PromptAsk    = "prompt" // server: an egress request waiting for an answer
	PromptAnswer = "answer" // client: a decision for one prompt
	PromptDone   = "done"   // server: the prompt was answered, expired, or the answer refused
)

// The decisions a client may answer with, as egress.Decision's values.
const (
	DecisionAllowOnce   = "allow-once"
	DecisionAllowAlways = "allow-always"
	DecisionDeny        = "deny"
	DecisionDenyAlways  = "deny-always"
)

// ValidDecision reports whether d is a decision a client may send.
func ValidDecision(d string) bool {
	switch d {
	case DecisionAllowOnce, DecisionAllowAlways, DecisionDeny, DecisionDenyAlways:
		return true
	}
	return false
}

// Prompt is one message on the prompt stream: an ask (Type PromptAsk, with
// VM, Host, Port and Method), an answer (PromptAnswer, with Decision), or the
// server's done (PromptDone, with Decision when answered, or Error: the
// prompt expired, or the answer was refused). The host and method can come
// from the guest: they are data.
type Prompt struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	VM       string `json:"vm,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Method   string `json:"method,omitempty"`
	Decision string `json:"decision,omitempty"`
	Error    *Error `json:"error,omitempty"`
}
