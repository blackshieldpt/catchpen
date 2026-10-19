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
				dropped = append(dropped, k)
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
		if err := walk(filepath.Join(dir, sub), sub, map[string]bool{}, add); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), dropped, nil
}

// walk adds regular files under path (following symlinks, skipping loops and
// dot-directories like .git) with tar names under prefix.
func walk(path, prefix string, seen map[string]bool, add func(string, []byte) error) error {
	real, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
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
		return add(prefix, b)
	}
	if !st.IsDir() || seen[real] {
		return nil
	}
	seen[real] = true
	defer delete(seen, real)
	ents, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") && e.IsDir() {
			continue
		}
		if err := walk(filepath.Join(real, e.Name()), prefix+"/"+e.Name(), seen, add); err != nil {
			return err
		}
	}
	return nil
}
