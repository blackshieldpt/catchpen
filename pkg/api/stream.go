package api

import "fmt"

// A terminal or console stream is a WebSocket. Binary messages carry its
// bytes, in both directions; what the server sends came from the guest, as
// it wrote it. Text messages carry one Frame each, as JSON.

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
