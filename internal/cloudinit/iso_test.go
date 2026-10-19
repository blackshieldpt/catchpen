package cloudinit

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readISO is a minimal ISO 9660 + Rock Ridge reader for the root directory.
func readISO(t *testing.T, img []byte) (label string, files map[string]string) {
	t.Helper()
	pvd := img[pvdSector*sector:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatal("no primary volume descriptor")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:]); int(got)*sector != len(img) {
		t.Fatalf("volume size %d sectors, image %d bytes", got, len(img))
	}
	root := pvd[156:]
	ext, size := binary.LittleEndian.Uint32(root[2:]), binary.LittleEndian.Uint32(root[10:])
	files = map[string]string{}
	dir := img[ext*sector : ext*sector+size]
	for off := 0; off < len(dir); {
		n := int(dir[off])
		if n == 0 { // rest of this sector is padding
			off = (off/sector + 1) * sector
			continue
		}
		r := dir[off : off+n]
		if off/sector != (off+n-1)/sector {
			t.Fatalf("record at %d crosses a sector boundary", off)
		}
		off += n
		idLen := int(r[32])
		if r[25]&2 != 0 {
			continue // . and ..
		}
		su := r[33+idLen+(1-idLen%2):]
		name := ""
		for len(su) >= 4 && su[2] >= 4 {
			if string(su[:2]) == "NM" {
				name = string(su[5:su[2]])
			}
			su = su[su[2]:]
		}
		e, l := binary.LittleEndian.Uint32(r[2:]), binary.LittleEndian.Uint32(r[10:])
		files[name] = string(img[e*sector : e*sector+l])
	}
	return strings.TrimRight(string(pvd[40:72]), " "), files
}

func TestWriteSeedReadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seed.iso")
	want := map[string]string{
		"user-data":      "#cloud-config\n",
		"meta-data":      "instance-id: x\n",
		"network-config": strings.Repeat("n", 3*sector+1),
		"vendor-data":    "",
	}
	files := map[string][]byte{}
	for k, v := range want {
		files[k] = []byte(v)
	}
	if err := WriteSeed(p, files); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("seed mode: %v %v", fi, err)
	}
	img, _ := os.ReadFile(p)
	label, got := readISO(t, img)
	if label != "cidata" {
		t.Errorf("label = %q", label)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("files = %q, want %q", got, want)
	}

	// Cross-check with the system tools cloud-init's environment relies on, when present.
	if _, err := exec.LookPath("isoinfo"); err == nil {
		out, _ := exec.Command("isoinfo", "-d", "-i", p).CombinedOutput()
		for _, s := range []string{"Volume id: cidata", "Rock Ridge signatures version 1 found"} {
			if !strings.Contains(string(out), s) {
				t.Errorf("isoinfo -d: missing %q in\n%s", s, out)
			}
		}
		for name, content := range want {
			out, err := exec.Command("isoinfo", "-R", "-x", "/"+name, "-i", p).Output()
			if err != nil || string(out) != content {
				t.Errorf("isoinfo -R -x /%s = %q, %v", name, out, err)
			}
		}
	}
	if _, err := exec.LookPath("blkid"); err == nil {
		out, err := exec.Command("blkid", "-o", "value", "-s", "LABEL", p).Output()
		if err != nil || strings.TrimSpace(string(out)) != "cidata" {
			t.Errorf("blkid label = %q, %v", out, err)
		}
	}
}

func TestSeedManyFilesSpanSectors(t *testing.T) {
	files := map[string][]byte{}
	want := map[string]string{}
	for i := range 40 {
		name := fmt.Sprintf("file-%02d-%s", i, strings.Repeat("x", 30))
		files[name] = []byte(name)
		want[name] = name
	}
	img, err := buildISO("cidata", files, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, got := readISO(t, img); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %d files, want %d", len(got), len(want))
	}
}

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestSeedRejectsBadNames(t *testing.T) {
	for _, files := range []map[string][]byte{
		{"a/b": nil},
		{"..": nil},
		{"": nil},
		{"user-data": nil, "USER_DATA": nil},
	} {
		if _, err := buildISO("cidata", files, testTime); err == nil {
			t.Errorf("%v: expected error", files)
		}
	}
}
