package jsonl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/fsutil"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rotate does what Append does past its size limit.
func rotate(t *testing.T, path, next string) {
	t.Helper()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	write(t, path, next)
}

func appendTo(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// follow runs tailRotated in follow mode, calling onEOF once at the first end
// of file, until want lines arrived or 3 s passed.
func follow(t *testing.T, path string, want int, onEOF func()) []string {
	t.Helper()
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []string
	err := tailRotated(ctx, path, true, func(l string) error {
		got = append(got, l)
		if len(got) == want {
			cancel()
		}
		return nil
	}, func() { once.Do(onEOF) })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func lines(t *testing.T, read func(context.Context, string, bool, func(string) error) error, ctx context.Context, path string) ([]string, error) {
	t.Helper()
	var got []string
	err := read(ctx, path, false, func(l string) error { got = append(got, l); return nil })
	return got, err
}

// The lines Append rotated out to path.1 come first; Tail, for files that
// never rotate, doesn't read path.1.
func TestTailRotatedReadsOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "a\nb\n")
	write(t, path, "c\n")
	if got, err := lines(t, TailRotated, context.Background(), path); err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %q, %v, want [a b c]", got, err)
	}
	if got, _ := lines(t, Tail, context.Background(), path); !slices.Equal(got, []string{"c"}) {
		t.Errorf("Tail read path.1: %q", got)
	}
}

// path and path.1 are opened under the writers' lock, so a rotation can't
// fall between the two opens (lines read twice, or a file skipped).
func TestTailRotatedHoldsWritersLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "x\n")
	write(t, path, "a\nb\n")
	unlock, err := fsutil.Lock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []string)
	go func() {
		got, _ := lines(t, TailRotated, context.Background(), path)
		done <- got
	}()
	select {
	case got := <-done:
		t.Fatalf("returned %q while the writers' lock was held", got)
	case <-time.After(200 * time.Millisecond):
	}
	rotate(t, path, "c\n")
	unlock()
	if got := <-done; !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %q, want [a b c]", got)
	}
}

// A line appended just before the rename, after the last read, is still read
// from the old file before the new one.
func TestTailFollowDrainsBeforeRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path, "a\n")
	got := follow(t, path, 3, func() { appendTo(t, path, "x\n"); rotate(t, path, "y\n") })
	if !slices.Equal(got, []string{"a", "x", "y"}) {
		t.Errorf("got %q, want [a x y]", got)
	}
}

// egress log -f after a rotation: path.1 first, then path, then across the
// next rotation, each line once.
func TestTailRotatedFollowFromOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "a\n")
	write(t, path, "b\n")
	got := follow(t, path, 4, func() { appendTo(t, path, "c\n"); rotate(t, path, "d\n") })
	if !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("got %q, want [a b c d]", got)
	}
}

// Two rotations while the follower lagged: the file in between is path.1,
// read before the newest.
func TestTailFollowTwoRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path, "a\n")
	got := follow(t, path, 4, func() { appendTo(t, path, "x\n"); rotate(t, path, "m\n"); rotate(t, path, "n\n") })
	if !slices.Equal(got, []string{"a", "x", "m", "n"}) {
		t.Errorf("got %q, want [a x m n]", got)
	}
}

// A last line without a newline is passed on when its file is rotated, as a
// non-follow read of the same file does.
func TestTailFollowEmitsPartialLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path, "a\ntrunc")
	got := follow(t, path, 3, func() { rotate(t, path, "new\n") })
	if !slices.Equal(got, []string{"a", "trunc", "new"}) {
		t.Errorf("got %q, want [a trunc new]", got)
	}
}

// With only path.1 there, -f shows it at once, not after path appears.
func TestTailRotatedFollowShowsOldFileFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "old\n")
	start := time.Now()
	var took time.Duration
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := TailRotated(ctx, path, true, func(l string) error { took = time.Since(start); cancel(); return nil })
	if err != nil || took == 0 || took > time.Second {
		t.Errorf("old line after %v (0: never), err %v", took, err)
	}
}

// A cancel while path.1 is read stops before path, with or without follow.
func TestTailRotatedStopsOnCancel(t *testing.T) {
	for _, follow := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "log")
		write(t, path+".1", "a\n")
		write(t, path, "b\n")
		ctx, cancel := context.WithCancel(context.Background())
		var got []string
		err := TailRotated(ctx, path, follow, func(l string) error { got = append(got, l); cancel(); return nil })
		cancel()
		if err != nil || !slices.Equal(got, []string{"a"}) {
			t.Errorf("follow %v: got %q, %v, want [a]", follow, got, err)
		}
	}
}

// path.1 that can't be read is an error, not silently skipped.
func TestTailRotatedOldFileError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "a\n")
	write(t, path, "b\n")
	if err := os.Chmod(path+".1", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := lines(t, TailRotated, context.Background(), path); err == nil {
		t.Errorf("unreadable path.1 skipped: got %q, nil", got)
	}
}

// Across a rotation, a last line without a newline in the newest file is
// passed on only if the writer is done (ErrWriterDone), as in Tail; a plain
// cancel leaves it out.
func TestTailFollowRotationCancel(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  []string
	}{
		{context.Canceled, []string{"a", "b"}},
		{ErrWriterDone, []string{"a", "b", "part"}},
	} {
		path := filepath.Join(t.TempDir(), "log")
		write(t, path, "a\n")
		ctx, cancel := context.WithCancelCause(context.Background())
		var once sync.Once
		var got []string
		err := tailRotated(ctx, path, true, func(l string) error { got = append(got, l); return nil },
			func() { once.Do(func() { rotate(t, path, "b\npart"); cancel(tc.cause) }) })
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("cause %v: got %q, %v, want %q", tc.cause, got, err, tc.want)
		}
	}
}

// A writer-done cancel while path.1 is read still reads path to its end.
func TestTailRotatedWriterDoneReadsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path+".1", "a\n")
	write(t, path, "b\npart")
	ctx, cancel := context.WithCancelCause(context.Background())
	var got []string
	err := TailRotated(ctx, path, true, func(l string) error { got = append(got, l); cancel(ErrWriterDone); return nil })
	if err != nil || !slices.Equal(got, []string{"a", "b", "part"}) {
		t.Errorf("got %q, %v, want [a b part]", got, err)
	}
}

// Tail, for a file that never rotates, keeps its own handling if the file is
// replaced: no lock file, no path.1, and on a plain cancel no line without a
// newline.
func TestTailFollowReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write(t, path, "a\npart")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []string
	err := Tail(ctx, path, true, func(l string) error {
		got = append(got, l)
		switch l {
		case "a":
			rotate(t, path, "b\n")
		case "b":
			cancel()
		}
		return nil
	})
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %q, %v, want [a b]", got, err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Tail created %s.lock: %v", path, err)
	}
}

// Following, a path.1 that can't be opened at a rotation is an error, as it
// is without follow, not a silent stall.
func TestTailFollowOldFileError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	path := filepath.Join(t.TempDir(), "log")
	write(t, path, "a\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var once sync.Once
	var got []string
	err := tailRotated(ctx, path, true, func(l string) error { got = append(got, l); return nil }, func() {
		once.Do(func() {
			rotate(t, path, "b\n")
			if err := os.Chmod(path+".1", 0); err != nil {
				t.Fatal(err)
			}
		})
	})
	if err == nil || ctx.Err() != nil {
		t.Errorf("got %q, %v (ctx %v), want an error before the timeout", got, err, ctx.Err())
	}
}
