package api

import (
	"encoding/json"
	"testing"
)

func TestCheckSize(t *testing.T) {
	for _, c := range []struct {
		w, h int
		ok   bool
	}{
		{80, 24, true}, {1, 1, true}, {MaxCols, MaxRows, true},
		{0, 24, false}, {80, 0, false}, {-1, 24, false}, {MaxCols + 1, 24, false}, {80, MaxRows + 1, false},
		{65616, 24, false}, // wraps to 80 as a uint16
	} {
		if err := CheckSize(c.w, c.h); (err == nil) != c.ok {
			t.Errorf("CheckSize(%d, %d) = %v", c.w, c.h, err)
		}
	}
}

// An exit status of 0 is sent, not left out.
func TestFrameJSON(t *testing.T) {
	zero := 0
	for f, want := range map[*Frame]string{
		{Type: FrameResize, W: 100, H: 40}:                                     `{"type":"resize","w":100,"h":40}`,
		{Type: FrameEnd, Exit: &zero}:                                          `{"type":"end","exit":0}`,
		{Type: FrameEnd, Error: &Error{Code: CodeVMPaused, Message: "paused"}}: `{"type":"end","error":{"code":"vm_paused","message":"paused"}}`,
	} {
		if b, err := json.Marshal(f); err != nil || string(b) != want {
			t.Errorf("%s, %v; want %s", b, err, want)
		}
	}
}
