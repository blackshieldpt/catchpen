package cloudinit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// ISO 9660 with a single root directory and Rock Ridge (RRIP 1.10) names.
const (
	sector    = 2048
	pvdSector = 16
	lPathSec  = 18
	mPathSec  = 19
	rootSec   = 20
)

// WriteSeed writes a NoCloud ISO (volume "cidata", Rock Ridge names) at dst.
// files is a flat map of file name to contents.
func WriteSeed(dst string, files map[string][]byte) error {
	iso, err := buildISO("cidata", files, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return fsutil.WriteFileAtomic(dst, iso, 0o600)
}

type isoFile struct {
	name, id string // Rock Ridge name, ISO 9660 identifier
	data     []byte
	extent   uint32
}

func buildISO(volume string, files map[string][]byte, now time.Time) ([]byte, error) {
	var fl []isoFile
	seen := map[string]string{}
	for name, b := range files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 200 {
			return nil, fmt.Errorf("invalid file name %q", name)
		}
		id := isoID(name)
		if other, dup := seen[id]; dup {
			return nil, fmt.Errorf("file names %q and %q collide", other, name)
		}
		seen[id] = name
		fl = append(fl, isoFile{name: name, id: id, data: b})
	}
	slices.SortFunc(fl, func(a, b isoFile) int { return strings.Compare(a.id, b.id) })

	// Root directory records first, to know how many sectors it spans.
	dirRecs := func(rootLen uint32) [][]byte {
		recs := [][]byte{
			dirRecord([]byte{0}, rootSec, rootLen, true, now, rootSU()),
			dirRecord([]byte{1}, rootSec, rootLen, true, now, px(0o40555, 2)),
		}
		for _, f := range fl {
			recs = append(recs, dirRecord([]byte(f.id), f.extent, uint32(len(f.data)), false, now, append(nm(f.name), px(0o100444, 1)...)))
		}
		return recs
	}
	rootLen := uint32(len(packDir(dirRecs(0))))
	next := rootSec + rootLen/sector
	for i := range fl {
		fl[i].extent = next
		next += uint32((len(fl[i].data) + sector - 1) / sector)
	}
	root := packDir(dirRecs(rootLen))
	total := next

	img := make([]byte, int(total)*sector)
	copy(img[pvdSector*sector:], pvd(volume, total, rootLen, now))
	term := img[(pvdSector+1)*sector:]
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1
	copy(img[lPathSec*sector:], pathTable(binary.LittleEndian))
	copy(img[mPathSec*sector:], pathTable(binary.BigEndian))
	copy(img[rootSec*sector:], root)
	for _, f := range fl {
		copy(img[int(f.extent)*sector:], f.data)
	}
	return img, nil
}

// isoID maps a name to an ISO 9660 level 2 identifier ("USER_DATA.;1").
func isoID(name string) string {
	b := []byte(strings.ToUpper(name))
	for i, c := range b {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '_'
		}
	}
	if len(b) > 28 {
		b = b[:28]
	}
	return string(b) + ".;1"
}

// packDir lays out records so none crosses a sector boundary; the result is
// padded to whole sectors.
func packDir(recs [][]byte) []byte {
	var buf bytes.Buffer
	for _, r := range recs {
		if rem := sector - buf.Len()%sector; len(r) > rem {
			buf.Write(make([]byte, rem))
		}
		buf.Write(r)
	}
	if rem := buf.Len() % sector; rem != 0 {
		buf.Write(make([]byte, sector-rem))
	}
	return buf.Bytes()
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func dirRecord(id []byte, extent, size uint32, dir bool, t time.Time, su []byte) []byte {
	n := 33 + len(id)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n, n+len(su)+1)
	r = append(r, su...)
	if len(r)%2 == 1 {
		r = append(r, 0)
	}
	r[0] = byte(len(r))
	both32(r[2:], extent)
	both32(r[10:], size)
	r[18], r[19], r[20] = byte(t.Year()-1900), byte(t.Month()), byte(t.Day())
	r[21], r[22], r[23] = byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	if dir {
		r[25] = 2
	}
	both16(r[28:], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}

// rootSU is the System Use area of the root "." record: SUSP SP, RRIP ER, PX.
func rootSU() []byte {
	su := []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0}
	id, des, src := "RRIP_1991A", "ROCK RIDGE INTERCHANGE PROTOCOL", ""
	su = append(su, 'E', 'R', byte(8+len(id)+len(des)+len(src)), 1, byte(len(id)), byte(len(des)), byte(len(src)), 1)
	su = append(su, id+des+src...)
	return append(su, px(0o40555, 2)...)
}

func px(mode, nlink uint32) []byte {
	b := make([]byte, 36)
	copy(b, []byte{'P', 'X', 36, 1})
	both32(b[4:], mode)
	both32(b[12:], nlink)
	return b // uid, gid 0
}

func nm(name string) []byte {
	return append([]byte{'N', 'M', byte(5 + len(name)), 1, 0}, name...)
}

func pathTable(o binary.ByteOrder) []byte {
	b := make([]byte, 10)
	b[0] = 1 // root identifier length
	o.PutUint32(b[2:], rootSec)
	o.PutUint16(b[6:], 1)
	return b
}

func decDate(t time.Time) []byte {
	return append([]byte(t.Format("20060102150405")+"00"), 0)
}

func pvd(volume string, total, rootLen uint32, now time.Time) []byte {
	b := make([]byte, sector)
	b[0] = 1
	copy(b[1:], "CD001")
	b[6] = 1
	pad := func(off, n int, s string) { copy(b[off:off+n], s+strings.Repeat(" ", n-len(s))) }
	pad(8, 32, "")
	pad(40, 32, volume)
	both32(b[80:], total)
	both16(b[120:], 1)
	both16(b[124:], 1)
	both16(b[128:], sector)
	both32(b[132:], 10)
	binary.LittleEndian.PutUint32(b[140:], lPathSec)
	binary.BigEndian.PutUint32(b[148:], mPathSec)
	copy(b[156:], dirRecord([]byte{0}, rootSec, rootLen, true, now, nil))
	for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		pad(f[0], f[1], "")
	}
	copy(b[813:], decDate(now))
	copy(b[830:], decDate(now))
	copy(b[847:], "0000000000000000")
	copy(b[864:], decDate(now))
	b[881] = 1
	return b
}
