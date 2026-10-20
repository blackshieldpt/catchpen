package safetar

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, link, body string
	typ              byte
}

func archive(t *testing.T, es []entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o777, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			w.Write([]byte(e.body))
		}
	}
	w.Close()
	return &b
}

func TestExtract(t *testing.T) {
	outside := t.TempDir()
	dest := t.TempDir()
	a := archive(t, []entry{
		{name: ".claude/projects/", typ: tar.TypeDir},
		{name: ".claude/projects/p/s.jsonl", body: "line\n", typ: tar.TypeReg},
		{name: "evil-link", link: outside, typ: tar.TypeSymlink},
		{name: "evil-link/pwned", body: "x", typ: tar.TypeReg},                // through the (skipped) symlink: a plain new dir instead
		{name: "../escape", body: "x", typ: tar.TypeReg},                      // traversal
		{name: "a/../../escape2", body: "x", typ: tar.TypeReg},                // traversal after cleaning
		{name: "/abs", body: "x", typ: tar.TypeReg},                           // absolute
		{name: "hard", link: "/etc/passwd", typ: tar.TypeLink},                // hard link
		{name: ".claude/projects/p/s.jsonl", body: "again", typ: tar.TypeReg}, // overwrite attempt
		{name: "dev", typ: tar.TypeChar},
	})
	res, err := Extract(a, dest, Limits{Bytes: 1 << 20, Files: 100})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, ".claude/projects/p/s.jsonl")); string(b) != "line\n" {
		t.Errorf("file = %q", b)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside dest: %v", ents)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "escape")); err == nil {
		t.Error("traversal escaped")
	}
	if st, err := os.Lstat(filepath.Join(dest, "evil-link")); err == nil && st.Mode()&os.ModeSymlink != 0 {
		t.Error("symlink created")
	}
	if st, _ := os.Stat(filepath.Join(dest, ".claude/projects/p/s.jsonl")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	skipped := strings.Join(res.Skipped, ",")
	for _, s := range []string{"evil-link", "../escape", "/abs", "hard", "dev"} {
		if !strings.Contains(skipped, s) {
			t.Errorf("%s not reported as skipped: %v", s, res.Skipped)
		}
	}
}

func TestExtractLimits(t *testing.T) {
	big := archive(t, []entry{{name: "a", body: strings.Repeat("x", 100), typ: tar.TypeReg}})
	if _, err := Extract(big, t.TempDir(), Limits{Bytes: 50, Files: 10}); err == nil {
		t.Error("byte limit not enforced")
	}
	var many []entry
	for i := 0; i < 20; i++ {
		many = append(many, entry{name: strings.Repeat("d", i+1), typ: tar.TypeDir})
	}
	if _, err := Extract(archive(t, many), t.TempDir(), Limits{Bytes: 50, Files: 10}); err == nil {
		t.Error("count limit not enforced")
	}
}

func TestSkippedCapped(t *testing.T) {
	var es []entry
	for i := 0; i < 50; i++ {
		es = append(es, entry{name: strings.Repeat("l", i+1), link: "/etc", typ: tar.TypeSymlink})
	}
	res, err := Extract(archive(t, es), t.TempDir(), Limits{Bytes: 1 << 20, Files: 100})
	if err != nil || res.NSkipped != 50 || len(res.Skipped) != maxSkippedNames {
		t.Errorf("skipped %d (%d names), %v", res.NSkipped, len(res.Skipped), err)
	}
}
