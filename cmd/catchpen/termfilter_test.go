package main

import (
	"bytes"
	"testing"
)

func TestTermFilter(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text\r\n", "plain text\r\n"},
		{"\x1b[1;31mred\x1b[0m", "\x1b[1;31mred\x1b[0m"},          // CSI kept
		{"a\x1b]0;evil title\x07b", "ab"},                         // OSC with BEL
		{"a\x1b]52;c;ZXZpbA==\x1b\\b", "ab"},                      // OSC 52 clipboard with ST
		{"a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},                        // DCS
		{"a\x1b_apc\x1b\\b\x1b^pm\x1b\\c\x1bXsos\x1b\\d", "abcd"}, // APC, PM, SOS
		{"a\xc2\x9d0;t\x07b", "ab"},                               // UTF-8 encoded C1 OSC
		{"a\xc2\x9bb", "ab"},                                      // other C1 dropped
		{"caf\xc3\xa9 \xc2\xa9", "caf\xc3\xa9 \xc2\xa9"},          // UTF-8 text kept
		{"a\x1b\x1b]0;T\x07b", "ab"},                              // ESC ESC: a terminal restarts the sequence
		{"a\xc2\x1b]0;T\x07b", "ab"},                              // lone 0xC2 before ESC
		{"a\x1b\n]0;T\x07b", "a\nb"},                              // C0 control inside the sequence
		{"a\x1b\x7f]0;T\x07b", "a\x7fb"},                          // DEL inside the sequence
		{"a\x1b\xc2\x9d0;T\x07b", "ab"},                           // ESC then UTF-8 C1 OSC
		{"a\xc2\xc2\x9d0;T\x07b", "ab"},                           // 0xC2 0xC2 then C1 OSC
		{"a\xc2\x9d\x07\x90b", "a\x90b"},                          // raw 0x90 after a dropped string
		{"a\xc2\xc2\x9d\x07\x90b", "a\x90b"},                      // no lone 0xC2 left to pair with a later 0x90
		{"a\x1b\x18]b", "a\x18]b"},                                // CAN cancels the sequence
		{"a\x1b\x1a]b", "a\x1a]b"},                                // SUB cancels the sequence
		{"a\x1b\x80]0;T\x07b", "a\x80]0;T\x07b"},                  // tmux stays in the sequence on 0x80-0xff
		{"a\x1b\xc3\x9d]0;T\x07b", "a\xc3\x9d]0;T\x07b"},          // the same with valid UTF-8 (Ý)
		{"a\x1b\x1b[1mb", "a\x1b[1mb"},                            // CSI after a doubled ESC still works
	}
	for _, c := range cases {
		var b bytes.Buffer
		f := &termFilter{w: &b}
		if _, err := f.Write([]byte(c.in)); err != nil {
			t.Fatal(err)
		}
		if b.String() != c.want {
			t.Errorf("%q -> %q, want %q", c.in, b.String(), c.want)
		}
	}
}

func TestTermFilterAcrossWrites(t *testing.T) {
	var b bytes.Buffer
	f := &termFilter{w: &b}
	for _, chunk := range []string{"x\x1b", "]0;ti", "tle\x1b", "\\y\xc2", "\x9d2;z\x07w"} {
		f.Write([]byte(chunk))
	}
	if b.String() != "xyw" {
		t.Fatalf("got %q", b.String())
	}
}

// opensString reports whether a terminal (DEC parser, UTF-8) fed b would
// start an OSC, DCS, SOS, PM or APC string.
func opensString(b []byte) bool {
	const (
		ground = iota
		esc
		escInter
	)
	st := ground
	isString := func(c byte) bool { return c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_' }
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0xc2 && i+1 < len(b) && b[i+1] >= 0x80 && b[i+1] <= 0x9f {
			i++
			switch b[i] {
			case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
				return true
			}
			st = ground
			continue
		}
		switch {
		case c == 0x1b:
			st = esc
		case c == 0x18 || c == 0x1a:
			st = ground
		case c < 0x20 || c == 0x7f:
			// executed or ignored; the escape state is kept
		case c >= 0x80 && st != ground:
			// tmux ignores 0x80-0xff inside an escape sequence and stays in it
		case st == esc && isString(c):
			return true
		case st == esc && c >= 0x20 && c <= 0x2f:
			st = escInter
		case st == escInter && c >= 0x20 && c <= 0x2f:
		default:
			st = ground
		}
	}
	return false
}

func FuzzTermFilter(f *testing.F) {
	for _, s := range []string{"a\x1b]0;t\x07b", "\x1b\x1b]0;T\x07", "\xc2\x1b]0;T\x07", "\x1b\n]0;T\x07",
		"\x1b\x7f]0;T\x07", "\x1b\xc2\x9d0;T\x07", "\xc2\xc2\x9d0;T\x07", "\x1b[1;31mred", "caf\xc3\xa9",
		"\x1b\x80]0;T\x07", "\x1b\xc3\x9d]0;T\x07"} {
		f.Add([]byte(s), 3)
	}
	f.Fuzz(func(t *testing.T, in []byte, split int) {
		var out bytes.Buffer
		tf := &termFilter{w: &out}
		if split > 0 && split < len(in) {
			tf.Write(in[:split])
			tf.Write(in[split:])
		} else {
			tf.Write(in)
		}
		if opensString(out.Bytes()) {
			t.Fatalf("%q -> %q opens a string", in, out.Bytes())
		}
	})
}
