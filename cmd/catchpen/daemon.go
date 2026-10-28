package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/daemon"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

func daemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run the background jobs: warm pool, idle stop, scheduled rebuilds, desktop egress prompts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := &daemon.Daemon{
				Manager: manager,
				Prompts: func(ctx context.Context, m *vm.Manager, dirs map[string]string) error {
					return egress.Watch(ctx, dirs, promptDecider(m, egress.DesktopDecider()))
				},
				Log: os.Stderr,
			}
			return d.Run(cmd.Context())
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start catchpen daemon as a systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if err := daemon.Install(m.Bin); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "installed %s and started it (logs: journalctl --user -u catchpen)\n", daemon.UnitPath())
			return nil
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.Uninstall()
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether catchpen daemon runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			running := "not running"
			if daemon.Running(m.P) {
				running = "running"
			}
			unit := daemon.UnitState()
			if unit == "" {
				unit = "not installed"
			}
			fmt.Printf("daemon: %s\nservice: %s (%s)\n", running, unit, daemon.UnitPath())
			return nil
		},
	}
	c.AddCommand(install, uninstall, status)
	return c
}

func poolCmd() *cobra.Command {
	c := &cobra.Command{Use: "pool", Short: "Manage the warm pool (config.yaml pool:)"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List pool VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			vms, err := m.PoolVMs()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tTEMPLATE\tDISTRO\tCPU\tMEM\tAGE")
			for _, v := range vms {
				state := "filling"
				switch {
				case v.Hit:
					state = "canary hit (catchpen destroy " + v.Name + ")"
				case v.Ready():
					state = "ready"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%dM\t%s\n", v.Name, state, v.Template, v.Distro, v.CPUs, v.MemoryMB, age(v.Created))
			}
			return w.Flush()
		},
	}
	fill := &cobra.Command{
		Use:   "fill",
		Short: "Replace stale pool VMs and create the missing ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if len(m.Cfg.Pool) == 0 {
				fmt.Fprintln(os.Stderr, "no pool: in config.yaml; removing any pool VMs")
			}
			n, err := m.FillPool(cmd.Context())
			fmt.Fprintf(os.Stderr, "added %d pool VM(s)\n", n)
			return err
		},
	}
	drain := &cobra.Command{
		Use:   "drain",
		Short: "Destroy all pool VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			n, err := m.DrainPool()
			if errors.Is(err, vm.ErrFillRunning) {
				return fmt.Errorf("%w; try again when it is done", err)
			}
			fmt.Fprintf(os.Stderr, "removed %d pool VM(s)\n", n)
			return err
		},
	}
	c.AddCommand(ls, fill, drain)
	return c
}
