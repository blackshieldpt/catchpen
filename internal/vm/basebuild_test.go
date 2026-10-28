package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackshieldpt/catchpen/internal/cloudinit"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vmtest"
)

// fakeBaseBuild stands in for building r's base: a cached distro image, and a
// qemu-system-x86_64 on PATH that copies the build seed to the returned path
// and reports success on the build log.
func fakeBaseBuild(t *testing.T, m *Manager, r *Resolved) (seedCopy string) {
	t.Helper()
	needQemuImg(t)
	img := filepath.Join(m.P.Images(), r.Distro.Name+".img")
	if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", img, "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	digest := "sha256:" + strings.ToLower(r.Distro.SHA256)
	if r.Distro.SHA256 == "" {
		digest = "sha256:00"
	}
	meta, _ := json.Marshal(map[string]string{"url": r.Distro.URL, "digest": digest})
	if err := os.WriteFile(img+".json", meta, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	seedCopy = filepath.Join(t.TempDir(), "seed.iso") // no spaces or commas: TMPDIR is plain
	script := "#!/bin/bash\nfor a in \"$@\"; do\n  case \"$a\" in\n" +
		"    file=*.seed.iso,*) f=${a#file=}; cp \"${f%%,*}\" " + seedCopy + " ;;\n" +
		"    file:*) echo " + cloudinit.BuildOK + " >>\"${a#file:}\" ;;\n" +
		"  esac\ndone\n"
	if err := os.WriteFile(filepath.Join(bin, "qemu-system-x86_64"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return seedCopy
}

// A pool VM can be the first to need its template's base. The base is shared
// with every later VM of the template, so it must be built from the whole
// template, agents included, not from the stripped copy a pool VM runs.
func TestPoolFillBuildsTheWholeBase(t *testing.T) {
	m := testManager(t)
	vmtest.Template(t, m.P.Templates(), "ag", "", "runtimes: { node: \"24\" }\nagents:\n  claude: {}\n")
	r, err := m.Resolve("ag")
	if err != nil {
		t.Fatal(err)
	}
	seed := fakeBaseBuild(t, m, r)
	s, err := m.Create(context.Background(), "p", CreateOpts{Template: "ag", pool: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents.Any() {
		t.Errorf("the pool VM itself has agents: %+v", s.Agents)
	}
	b, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("no base was built: %v", err)
	}
	pkg := template.AgentDefs[0].Package // claude
	if !bytes.Contains(b, []byte(pkg)) {
		t.Errorf("the base build doesn't install %s", pkg)
	}
}
