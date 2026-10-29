package vm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/cloudinit"
)

// fakeBuildEgress makes the fake build of fakeBaseBuild also write the
// build's egress log, and with rotated the file a rotation moved aside, into
// the build's .egress directory (next to the seed: <stem>.qcow2.seed.iso).
// With fail the build reports no result and qemu exits 1.
func fakeBuildEgress(t *testing.T, rotated, fail bool) {
	t.Helper()
	old := ""
	if rotated {
		old = ` printf 'old\n' >"$d/egress.log.1";`
	}
	ok := "    file:*) echo " + cloudinit.BuildOK + " >>\"${a#file:}\" ;;\n"
	if fail {
		ok = ""
	}
	bin := t.TempDir()
	script := "#!/bin/bash\nfor a in \"$@\"; do\n  case \"$a\" in\n" +
		"    file=*.seed.iso,*) f=${a#file=}; f=${f%%,*}; d=${f%.qcow2.seed.iso}.egress; printf 'new\\n' >\"$d/egress.log\";" + old + " ;;\n" +
		ok + "  esac\ndone\n"
	if fail {
		script += "exit 1\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "qemu-system-x86_64"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A base build keeps its egress log next to the base, with the file the
// log's rotation moved aside, whether or not it succeeds, and leaves no stale
// one from an earlier try.
func TestBaseBuildKeepsEgressLogs(t *testing.T) {
	for _, c := range []struct {
		name                 string
		egress, rotated, bad bool   // the build logs at all; its log rotated; it fails
		want, want1          string // <stem>.egress.log and .egress.log.1 afterwards ("" = absent)
	}{
		{"rotated", true, true, false, "new\n", "old\n"},
		{"stale old file", true, false, false, "new\n", ""},
		{"failed build", true, true, true, "new\n", "old\n"},
		{"no egress log", false, false, false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := testManager(t)
			r, err := m.Resolve("minimal")
			if err != nil {
				t.Fatal(err)
			}
			fakeBaseBuild(t, m, r)
			if c.egress {
				fakeBuildEgress(t, c.rotated, c.bad)
			}
			stem := strings.TrimSuffix(r.basePath(m.P, 1), ".qcow2")
			// An earlier failed try at the same base left its logs.
			writeFile(t, stem+".egress.log", "stale\n")
			writeFile(t, stem+".egress.log.1", "stale\n")
			dst, err := m.EnsureBase(context.Background(), r, false)
			switch {
			case c.bad && (err == nil || !strings.Contains(err.Error(), "base build failed")):
				t.Fatalf("EnsureBase = %v; want a failed build", err)
			case !c.bad && err != nil:
				t.Fatal(err)
			case !c.bad && strings.TrimSuffix(dst, ".qcow2") != stem:
				t.Fatalf("built %s, want %s.qcow2", dst, stem)
			}
			for _, f := range []struct{ name, want string }{{".egress.log", c.want}, {".egress.log.1", c.want1}} {
				b, err := os.ReadFile(stem + f.name)
				switch {
				case f.want == "" && err == nil:
					t.Errorf("%s = %q; want none (this build left none)", f.name, b)
				case f.want != "" && (err != nil || string(b) != f.want):
					t.Errorf("%s = %q, %v; want %q", f.name, b, err, f.want)
				}
			}
		})
	}
}

// gc removes the rotated-out egress log with the rest of a base's logs, and an
// old one a failed build left, but keeps the current base's.
func TestGCRemovesOldBuildEgressLog(t *testing.T) {
	m := testManager(t)
	r, err := m.Resolve("minimal")
	if err != nil {
		t.Fatal(err)
	}
	g1, g2 := r.basePath(m.P, 1), r.basePath(m.P, 2)
	writeFile(t, g1, "base")
	writeFile(t, g2, "base")
	if err := r.recordSpec(m.P); err != nil {
		t.Fatal(err)
	}
	removed := strings.TrimSuffix(g1, ".qcow2") + ".egress.log.1" // of an unused base
	current := strings.TrimSuffix(g2, ".qcow2") + ".egress.log.1"
	failed := filepath.Join(m.P.Bases(), "minimal-fedcba9876543210.egress.log.1") // a build that never produced a base
	// All old: what keeps the current base's is its base, not its age.
	oldTime := time.Now().Add(-gcTempAge - time.Hour)
	for _, p := range []string{removed, current, failed} {
		writeFile(t, p, "log")
		os.Chtimes(p, oldTime, oldTime)
	}
	items, err := m.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, filepath.Base(it.Path)+" ("+it.Why+")")
	}
	for _, want := range []string{filepath.Base(removed) + " (log of a removed base)", filepath.Base(failed) + " (log of a failed build)"} {
		if !slices.Contains(got, want) {
			t.Errorf("gc items %q lack %q", got, want)
		}
	}
	for _, p := range []string{removed, failed} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s not removed", filepath.Base(p))
		}
	}
	if _, err := os.Stat(current); err != nil {
		t.Errorf("the current base's %s removed", filepath.Base(current))
	}
}
