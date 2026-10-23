package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blackshieldpt/curral/internal/safetar"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
)

// Agent session stores copied as transcripts, relative to the guest home.
// Credentials (claude's .credentials.json, opencode's auth.json, pi's
// auth.json, omp's agent.db and .env, codex's auth.json) live outside these paths.
var transcriptPaths = func() (out []string) {
	for _, d := range template.AgentDefs {
		out = append(out, d.Transcripts...)
	}
	return out
}()

var transcriptLimits = safetar.Limits{Bytes: 1 << 30, Files: 200_000}

// CopyTranscripts copies the agents' session stores from a running VM to
// transcripts/<vm>/<time>/. The archive comes from the guest, so only
// regular files and directories are unpacked. It returns the directory, or
// "" when there was nothing to copy.
func (m *Manager) CopyTranscripts(ctx context.Context, name string) (string, error) {
	g, _, err := m.DialGuest(ctx, name)
	if err != nil {
		return "", err
	}
	defer g.Close()
	c := g.Client
	dest := filepath.Join(m.P.Transcripts(), name, time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	var q []string
	for _, p := range transcriptPaths {
		q = append(q, shQuote(p))
	}
	cmd := `cd "$HOME" && set -- && for p in ` + strings.Join(q, " ") + `; do [ -e "$p" ] && set -- "$@" "$p"; done; [ $# -eq 0 ] || exec tar -cf - -- "$@"`
	pr, pw := io.Pipe()
	type result struct {
		res safetar.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := safetar.Extract(pr, dest, transcriptLimits)
		if err == nil {
			_, _ = io.Copy(io.Discard, pr) // tar's padding after the end of the archive
		}
		pr.CloseWithError(err) // stops the stream early on a limit
		done <- result{res, err}
	}()
	stderr, serr := sshx.Stream(ctx, c, cmd, pw, transcriptLimits.Bytes+64<<20)
	pw.CloseWithError(serr)
	r := <-done
	if r.err == nil && serr != nil {
		r.err = fmt.Errorf("%w: %s", serr, stderr)
	}
	if r.err != nil {
		return dest, fmt.Errorf("transcripts of %s: %w", name, r.err)
	}
	if r.res.NSkipped > 0 {
		m.logf("warning: transcripts of %s: skipped %d entries that weren't plain files (e.g. %s)", name, r.res.NSkipped, r.res.Skipped[0])
	}
	if r.res.Files == 0 {
		_ = os.Remove(dest)
		return "", nil
	}
	m.logf("copied %d transcript files (%s) from %s to %s", r.res.Files, humanMB(r.res.Bytes), name, dest)
	return dest, nil
}

func humanMB(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// TranscriptDirs lists a VM's transcript copies, oldest first. They outlive the VM.
func (m *Manager) TranscriptDirs(name string) ([]string, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(m.P.Transcripts(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, filepath.Join(m.P.Transcripts(), name, e.Name()))
		}
	}
	return out, nil
}
