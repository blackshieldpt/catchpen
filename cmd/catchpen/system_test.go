package main

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// catchpen console sends what is typed up to Ctrl-], and nothing after it.
func TestDetachReader(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ls\r", "ls\r"},
		{"ls\r\x1dreboot\r", "ls\r"},
		{"\x1dreboot\r", ""},
	} {
		for _, r := range []io.Reader{strings.NewReader(c.in), iotest.OneByteReader(strings.NewReader(c.in))} {
			b, err := io.ReadAll(detachReader{r})
			if err != nil || string(b) != c.want {
				t.Errorf("%q: sent %q, %v; want %q", c.in, b, err, c.want)
			}
		}
	}
}
