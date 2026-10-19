package agents

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxConfigBytes = 64 << 20
	maxConfigFiles = 20000
)

// settingsDrop are settings.json keys not copied into VMs: credential helpers,
// host commands (hooks, status line) and plugins, which are not synced.
var settingsDrop = []string{
	"env", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh",
	"otelHeadersHelper", "hooks", "statusLine", "enabledPlugins", "extraKnownMarketplaces",
}

// claudeConfigTar packs settings.json (sanitized), CLAUDE.md and the agents,
// skills and commands directories of a host ~/.claude. Symlinks are followed;
// credentials, history and projects are never included.
func claudeConfigTar(dir string) ([]byte, []string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	files := 0
	add := func(name string, data []byte) error {
		total += int64(len(data))
		files++
		if total > maxConfigBytes || files > maxConfigFiles {
			return fmt.Errorf("%s is too large to sync (limit %d MiB, %d files)", dir, maxConfigBytes>>20, maxConfigFiles)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	var dropped []string
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, nil, fmt.Errorf("settings.json: %w", err)
		}
		for _, k := range settingsDrop {
			if _, ok := doc[k]; ok {
				delete(doc, k)
				dropped = append(dropped, "settings.json "+k)
			}
		}
		out, _ := json.MarshalIndent(doc, "", "  ")
		if err := add("settings.json", append(out, '\n')); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); err == nil {
		if err := add("CLAUDE.md", b); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, sub := range []string{"agents", "skills", "commands"} {
		root, err := filepath.EvalSymlinks(filepath.Join(dir, sub))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if sensitive(root) {
			dropped = append(dropped, sub+" (points into a credentials directory)")
			continue
		}
		w := walker{root: root, add: add, seen: map[string]bool{}}
		if err := w.walk(root, sub); err != nil {
			return nil, nil, err
		}
		dropped = append(dropped, w.skipped...)
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), dropped, nil
}

// sensitiveDirs (relative to the host home) are never copied, even via a symlink.
var sensitiveDirs = []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".password-store",
	".config/gh", ".config/gcloud", ".config/curral", ".local/share/curral"}

// sensitive reports whether a resolved path is the home directory itself or
// inside a credentials directory. It is checked for every visited path.
func sensitive(real string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if real == home {
		return true
	}
	for _, d := range sensitiveDirs {
		p := filepath.Join(home, d)
		if real == p || strings.HasPrefix(real, p+"/") {
			return true
		}
	}
	return false
}

// walker adds the regular files under root. Symlinks are followed only while
// they stay inside root; dotfiles and dot-directories (.git, .credentials.json,
// .env, ...) are skipped.
type walker struct {
	root    string
	add     func(string, []byte) error
	seen    map[string]bool
	skipped []string
}

func (w *walker) walk(path, name string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		w.skipped = append(w.skipped, name+" (unreadable link)")
		return nil
	}
	if real != w.root && !strings.HasPrefix(real, w.root+"/") {
		w.skipped = append(w.skipped, name+" (links outside its directory)")
		return nil
	}
	if sensitive(real) {
		w.skipped = append(w.skipped, name+" (credentials directory)")
		return nil
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if st.Mode().IsRegular() {
		b, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		return w.add(name, b)
	}
	if !st.IsDir() || w.seen[real] {
		return nil
	}
	w.seen[real] = true
	defer delete(w.seen, real)
	ents, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := w.walk(filepath.Join(real, e.Name()), name+"/"+e.Name()); err != nil {
			return err
		}
	}
	return nil
}
