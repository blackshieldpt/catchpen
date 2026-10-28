package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/template"
)

//go:embed initfiles
var initFiles embed.FS

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write commented config.yaml, secrets.env and the template category files; existing files are kept",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeInitFiles(paths.Default(), cmd.OutOrStdout())
		},
	}
}

type initFile struct {
	dst     string
	content []byte
	mode    os.FileMode
}

func initFileList(p paths.Paths) ([]initFile, error) {
	var out []initFile
	for _, f := range []struct {
		src, dst string
		mode     os.FileMode
	}{
		{"config.yaml", p.ConfigFile(), 0o644},
		{"secrets.env", p.SecretsFile(), 0o600},
	} {
		b, err := initFiles.ReadFile("initfiles/" + f.src)
		if err != nil {
			return nil, err
		}
		out = append(out, initFile{f.dst, b, f.mode})
	}
	for _, c := range template.Categories {
		ex, err := initFiles.ReadFile("initfiles/examples/" + c.Name + ".yaml")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out = append(out, initFile{filepath.Join(p.Templates(), c.Name+".yaml"), categoryFile(c, string(ex)), 0o644})
	}
	return out, nil
}

// categoryFile is a commented category file: what it's for, catchpen's built-in
// sections for reference, and examples.
func categoryFile(c template.Category, examples string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s.yaml: %s.\n", c.Name, c.About)
	if c.Fields != nil {
		fmt.Fprintf(&b, "## Each section is a name with settings (%s, and a description).\n", strings.Join(c.Fields, ", "))
	} else {
		b.WriteString("## Each section is a name with a list of picks from any category, used like one: -t <name>.\n")
	}
	switch {
	case c.Fields == nil:
		b.WriteString("## A section here replaces a built-in one of the same name.\n")
	case c.Exclusive:
		b.WriteString("## -t picks one section here, which replaces the default.\n")
	default:
		b.WriteString("## -t picks add to the default; -<name> drops a section from it. A section can also be\n## a list of other sections here.\n")
	}
	if c.Fields != nil {
		b.WriteString(`## A section here replaces a built-in one of the same name, and "default:" replaces the
## built-in default.
`)
	}
	b.WriteString(`## To use one below, uncomment it with all the lines under it.
## Reference: https://blackshieldpt.github.io/catchpen/templates/
`)
	if bi := template.BuiltinFile(c.Name); bi != "" {
		b.WriteString("\n## Built in:\n")
		b.WriteString(commentLines(bi))
	}
	if strings.TrimSpace(examples) != "" {
		b.WriteString("\n## Examples:\n")
		b.WriteString(commentLines(examples))
	}
	return []byte(b.String())
}

func commentLines(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("# " + l + "\n")
	}
	return b.String()
}

func writeInitFiles(p paths.Paths, out io.Writer) error {
	for _, d := range []string{p.Config, p.Templates()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	files, err := initFileList(p)
	if err != nil {
		return err
	}
	for _, f := range files {
		w, err := os.OpenFile(f.dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode)
		if errors.Is(err, fs.ErrExist) {
			fmt.Fprintf(out, "kept     %s (exists)\n", f.dst)
			continue
		}
		if err != nil {
			return err
		}
		_, err = w.Write(f.content)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.dst, err)
		}
		fmt.Fprintf(out, "created  %s\n", f.dst)
	}
	if _, err := (template.Loader{UserDir: p.Templates()}).Library(); err != nil {
		fmt.Fprintf(out, "warning: %v\n", err) // e.g. a template file from before category files
	}
	return nil
}
