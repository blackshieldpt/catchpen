package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/paths"
)

//go:embed initfiles
var initFiles embed.FS

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write commented config.yaml, secrets.env and templates/example.yaml; existing files are kept",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeInitFiles(paths.Default(), cmd.OutOrStdout())
		},
	}
}

func writeInitFiles(p paths.Paths, out io.Writer) error {
	for _, d := range []string{p.Config, p.Templates()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		src, dst string
		mode     os.FileMode
	}{
		{"config.yaml", p.ConfigFile(), 0o644},
		{"secrets.env", p.SecretsFile(), 0o600},
		{"example.yaml", filepath.Join(p.Templates(), "example.yaml"), 0o644},
	} {
		b, err := initFiles.ReadFile("initfiles/" + f.src)
		if err != nil {
			return err
		}
		w, err := os.OpenFile(f.dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode)
		if errors.Is(err, fs.ErrExist) {
			fmt.Fprintf(out, "kept     %s (exists)\n", f.dst)
			continue
		}
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.dst, err)
		}
		fmt.Fprintf(out, "created  %s\n", f.dst)
	}
	return nil
}
