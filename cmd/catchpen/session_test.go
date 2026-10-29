package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlayRecordingSaysHowItEnds(t *testing.T) {
	const head = "{\"version\":2,\"width\":80,\"height\":24,\"timestamp\":1}\n"
	const cut = "[0.2,\"m\",\"recording stopped: size limit reached\"]\n"
	for _, c := range []struct {
		name, cast string
		tty        bool
		want       string // the line on errw
	}{
		// The guest leaves a title string open: the filter drops all that
		// follows, so the notice must not go through it.
		{"open string, then cut", head + `[0.1,"o","a\u001b]0;\u0000\u0000"]` + "\n" + cut, true, "catchpen: the recording stops here: it reached its size limit; the rest of the session was not recorded\n"},
		{"lone ESC, then cut", head + `[0.1,"o","a\u001b"]` + "\n" + cut, true, "catchpen: the recording stops here: it reached its size limit; the rest of the session was not recorded\n"},
		{"complete, guest fakes the notice", head + `[0.1,"o","catchpen: the recording stops here"]` + "\n", true, "catchpen: end of recording\n"},
		{"complete, not a terminal", head + `[0.1,"o","a"]` + "\n", false, "catchpen: end of recording\n"},
	} {
		var out, errw bytes.Buffer
		if err := playRecording(strings.NewReader(c.cast), &out, &errw, c.tty, 1, 0); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if errw.String() != c.want {
			t.Errorf("%s: stderr %q, want %q", c.name, errw.String(), c.want)
		}
		if got := strings.HasSuffix(out.String(), termReset); got != c.tty {
			t.Errorf("%s: output %q ends with the reset: %v, want %v", c.name, out.String(), got, c.tty)
		}
	}
}
