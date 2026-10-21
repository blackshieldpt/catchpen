package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, st Store, kind, url string, size int, used time.Time) string {
	t.Helper()
	p := st.path(kind, url)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(p), tmpPfx+"x")
	if err := os.WriteFile(tmp, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commit(tmp, p, meta{URL: url}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, used, used); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTrimLeastRecentlyUsed(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	now := time.Now()
	oldest := put(t, st, "npm", "u1", 100, now.Add(-3*time.Hour))
	old := put(t, st, "apt", "u2", 100, now.Add(-2*time.Hour))
	recent := put(t, st, "go", "u3", 100, now.Add(-time.Hour))
	leftover := filepath.Join(filepath.Dir(recent), tmpPfx+"dead")
	os.WriteFile(leftover, []byte("partial"), 0o600)
	os.Chtimes(leftover, now.Add(-2*tmpAge), now.Add(-2*tmpAge))
	fresh := filepath.Join(filepath.Dir(recent), tmpPfx+"live")
	os.WriteFile(fresh, []byte("partial"), 0o600)

	if n, b, err := st.Trim(150, true); err != nil || n != 2 || b != 200 {
		t.Fatalf("dry run: %d %d %v", n, b, err)
	}
	if _, err := os.Stat(oldest); err != nil {
		t.Fatal("dry run removed an object")
	}
	if n, b, err := st.Trim(150, false); err != nil || n != 2 || b != 200 {
		t.Fatalf("trim: %d %d %v", n, b, err)
	}
	for p, want := range map[string]bool{oldest: false, oldest + metaSfx: false, old: false, recent: true, recent + metaSfx: true, leftover: false, fresh: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	us, err := st.Usage()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if want := map[string]int{"go": 1}[u.Kind]; u.Files != want {
			t.Errorf("%s: %d files, want %d", u.Kind, u.Files, want)
		}
	}
}

func TestClear(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	a := put(t, st, "npm", "u1", 1, time.Now())
	b := put(t, st, "go", "u2", 1, time.Now())
	if err := st.Clear("maven"); err == nil {
		t.Error("unknown kind cleared")
	}
	if err := st.Clear("npm"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); err == nil {
		t.Error("npm not cleared")
	}
	if _, err := os.Stat(b); err != nil {
		t.Error("go cleared too")
	}
	if err := st.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b); err == nil {
		t.Error("clear without kinds keeps go")
	}
}
