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
