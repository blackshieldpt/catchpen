package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/forge"
)

// forge add's usage line and --mode help must name every auth mode, each as
// its own entry (token is also inside minted-token, so no substring checks).
func TestForgeAddHelpListsEveryMode(t *testing.T) {
	want := []string{forge.ModeDeployKey, forge.ModeMintedToken, forge.ModeToken, forge.ModeAgent, forge.ModeNone}
	var add *cobra.Command
	for _, c := range forgeCmd().Commands() {
		if c.Name() == "add" {
			add = c
		}
	}
	if add == nil {
		t.Fatal("no forge add command")
	}
	_, use, _ := strings.Cut(add.Use, "--mode ")
	help := strings.Split(add.Flags().Lookup("mode").Usage, " | ")
	for _, m := range want {
		if !slices.Contains(strings.Split(use, "|"), m) {
			t.Errorf("usage %q lacks mode %s", add.Use, m)
		}
		if !slices.Contains(help, m) {
			t.Errorf("--mode help %q lacks mode %s", help, m)
		}
	}
}
