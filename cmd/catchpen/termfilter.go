package main

import "io"

// termFilter passes a guest's serial output to the terminal but drops the
// escape sequences that act outside the screen contents: OSC (title,
// clipboard, hyperlinks), DCS, SOS, PM and APC strings, and their C1 forms.
// Cursor movement and colours (CSI) still work, so the console stays usable.
type termFilter struct {
	w     io.Writer
	state int
}

const (
	tfText   = iota
	tfEsc    // after ESC
	tfString // inside an OSC/DCS/SOS/PM/APC string: dropped
	tfStrEsc // ESC inside a string (maybe ST)
	tfC2     // after a 0xC2 lead byte (possible UTF-8 encoded C1 control)
)

func (t *termFilter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch t.state {
		case tfText:
			switch b {
			case 0x1b:
				t.state = tfEsc
			case 0xc2:
				t.state = tfC2
			default:
				out = append(out, b)
			}
		case tfEsc:
			switch b {
			case ']', 'P', 'X', '^', '_':
				t.state = tfString
			default:
				out = append(out, 0x1b, b)
				t.state = tfText
			}
		case tfString:
			switch b {
			case 0x07: // BEL ends OSC
				t.state = tfText
			case 0x1b:
				t.state = tfStrEsc
			}
		case tfStrEsc:
			if b == '\\' {
				t.state = tfText
			} else {
				t.state = tfString
			}
		case tfC2:
			switch {
			case b == 0x9d || b == 0x90 || b == 0x98 || b == 0x9e || b == 0x9f:
				t.state = tfString // C1 OSC/DCS/SOS/PM/APC
			case b >= 0x80 && b <= 0x9f:
				t.state = tfText // other C1 controls: drop
			default:
				out = append(out, 0xc2, b)
				t.state = tfText
			}
		}
	}
	if _, err := t.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
