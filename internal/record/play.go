package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Play writes a recording's output to w with its original timing (idle
// gaps capped at maxIdle, sped up by speed).
func Play(r io.Reader, w io.Writer, speed float64, maxIdle time.Duration) error {
	if speed <= 0 {
		speed = 1
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), MaxBytes)
	if !sc.Scan() {
		return errors.New("empty recording")
	}
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Version != 2 {
		return fmt.Errorf("not an asciicast v2 recording")
	}
	var last float64
	for sc.Scan() {
		var ev []any
		if json.Unmarshal(sc.Bytes(), &ev) != nil || len(ev) != 3 {
			continue
		}
		t, _ := ev[0].(float64)
		kind, _ := ev[1].(string)
		data, _ := ev[2].(string)
		d := time.Duration((t - last) / speed * float64(time.Second))
		if d > maxIdle {
			d = maxIdle
		}
		if d > 0 {
			time.Sleep(d)
		}
		last = t
		if kind == "o" {
			if _, err := io.WriteString(w, data); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// Info summarizes a recording file.
type Info struct {
	Header
	Duration time.Duration
	Size     int64
}

// Stat reads a recording's header and its last event time.
func Stat(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	in := Info{Size: st.Size()}
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return in, err
	}
	if err := json.Unmarshal(line, &in.Header); err != nil {
		return in, fmt.Errorf("%s: bad header", path)
	}
	const tail = 64 << 10
	off := st.Size() - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return in, err
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var ev []any
		if json.Unmarshal(lines[i], &ev) == nil && len(ev) == 3 {
			if t, ok := ev[0].(float64); ok {
				in.Duration = time.Duration(t * float64(time.Second))
				break
			}
		}
	}
	return in, nil
}
