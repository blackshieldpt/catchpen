package task

import (
	"io"
	"runtime"
	"strings"
	"testing"
)

// The cmd agent's output can be 512 MiB: only its last maxResult bytes are
// read into memory.
func TestSummarizeCmdKeepsOnlyTheTail(t *testing.T) {
	r := io.MultiReader(io.LimitReader(zeros{}, 256<<20), strings.NewReader("the end\n"))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := Summarize("cmd", r)
	runtime.ReadMemStats(&after)
	if len(s.Result) != maxResult || !strings.HasSuffix(s.Result, "the end\n") {
		t.Errorf("result: %d bytes, ends %q", len(s.Result), s.Result[max(0, len(s.Result)-8):])
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("allocated %d MiB for 256 MiB of output", grew>>20)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
