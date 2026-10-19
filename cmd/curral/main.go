// Command curral launches KVM sandboxes for coding agents.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/config"
	"github.com/blackshieldpt/curral/internal/paths"
	"github.com/blackshieldpt/curral/internal/template"
	"github.com/blackshieldpt/curral/internal/vm"
)

var debug bool

func manager() (*vm.Manager, error) {
	p := paths.Default()
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return nil, err
	}
	return &vm.Manager{
		P: p, Cfg: cfg, Tpl: template.Loader{UserDir: p.Templates()},
		Out: os.Stderr, In: os.Stdin, Bin: bin, Debug: debug,
	}, nil
}

func main() {
	root := &cobra.Command{
		Use:           "curral",
		Short:         "KVM sandboxes for coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&debug, "debug", false, "stream the whole base build log (not just progress); log the VM serial console and keep VMs that fail to boot running")
	root.AddCommand(
		doctorCmd(), templateCmd(), upCmd(), startCmd(), stopCmd(), destroyCmd(), lsCmd(),
		sshCmd(), sshProxyCmd(), consoleCmd(), egressCmd(), egressStdioCmd(), setupSSHCmd(), credsCmd(),
		forgeCmd(), pullCmd(), prCmd(), forwardCmd(),
	)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	// After the first signal, restore default handling so a second one kills curral.
	go func() { <-ctx.Done(); cancel() }()
	if err := root.ExecuteContext(ctx); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(int(ee))
		}
		fmt.Fprintln(os.Stderr, "curral:", err)
		os.Exit(1)
	}
}

// exitError propagates a child's exit status without printing.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
