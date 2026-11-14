package api

import "fmt"

// A terminal or console stream is a WebSocket. Binary messages carry its
// bytes, in both directions; what the server sends came from the guest, as
// it wrote it. Text messages carry one Frame each, as JSON.
//
// The server's last message is a Frame of type FrameEnd, with Exit and/or
// Error, and then it closes the connection normally. A resize is the one
// Frame a client sends.

// Frame types.
const (
	FrameResize = "resize" // client: the terminal's new size, in W and H
	FrameEnd    = "end"    // server: the session ended, with Exit or Error
)

// Frame is a control message on a stream.
type Frame struct {
	Type  string `json:"type"`
	W     int    `json:"w,omitempty"`
	H     int    `json:"h,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// PathVMTerminal is GET, upgraded to a WebSocket: an interactive terminal
// in the VM, as catchpen ssh, run and attach make one. Its query says which
// (see QueryKind and the Terminal kinds); the stream is recorded and
// audited as web-<kind>.
func PathVMTerminal(name string) string { return PathVM(name) + "/terminal" }

// PathVMConsole is GET, upgraded to a WebSocket: the VM's serial console,
// filtered as catchpen console filters it. One stream per VM.
func PathVMConsole(name string) string { return PathVM(name) + "/console" }

// A terminal stream's query parameters, all optional.
const (
	QueryKind  = "kind"  // one of the Terminal kinds; "ssh" by default
	QueryAgent = "agent" // kind=run: the agent, as catchpen run <vm> <agent>
	QueryArg   = "arg"   // kind=run: an argument, repeated once per argument
	QueryCols  = "cols"  // the terminal's width (with rows; default 80)
	QueryRows  = "rows"  // the terminal's height (with cols; default 24)
)

// The terminal kinds: what the terminal runs.
const (
	TerminalSSH    = "ssh"    // a login shell
	TerminalRun    = "run"    // catchpen run's command: the agent in its tmux session
	TerminalAttach = "attach" // catchpen attach's command: the main tmux session
)

// TerminalTerm is the TERM an API terminal runs with: the terminal an
// xterm.js cockpit emulates.
const TerminalTerm = "xterm-256color"

// MaxStreamMessage is the largest message a stream reads: a paste or a
// control frame bigger than this is refused by the WebSocket layer.
const MaxStreamMessage = 1 << 20

// The largest terminal a client may ask for.
const (
	MaxCols = 4096
	MaxRows = 4096
)

// CheckSize refuses a terminal size outside 1x1 to MaxCols x MaxRows.
func CheckSize(w, h int) error {
	if w < 1 || h < 1 || w > MaxCols || h > MaxRows {
		return fmt.Errorf("terminal size %dx%d is outside 1x1 to %dx%d", w, h, MaxCols, MaxRows)
	}
	return nil
}
