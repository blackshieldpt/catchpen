package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// confirm asks a yes/no question on the terminal.
func confirm(q string) error {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
		return errors.New("aborted")
	}
	return nil
}

func snapshotCmd() *cobra.Command {
	var diskOnly bool
	c := &cobra.Command{
		Use:   "snapshot <vm> [name]",
		Short: "Save a snapshot of a VM (with its memory when running)",
		Long: `Snapshots live inside the VM's disk. Of a running VM, the memory is saved too (the VM
pauses while it is written) so restore resumes processes where they were; --disk-only
saves just the disk. Memory snapshots hold whatever was in memory, credentials included.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			snap, err := m.Snapshot(cmd.Context(), args[0], name, diskOnly)
			if err == nil {
				fmt.Println(snap)
			}
			return err
		},
	}
	c.Flags().BoolVar(&diskOnly, "disk-only", false, "don't save the memory of a running VM")
	return c
}

func snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots <vm>",
		Short: "List a VM's snapshots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			ss, err := m.Snapshots(args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tCREATED\tMEMORY")
			for _, s := range ss {
				mem := "-"
				if s.VMStateMB > 0 {
					mem = humanBytes(s.VMStateMB << 20)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, s.Created.Local().Format("2006-01-02 15:04:05"), mem)
			}
			return w.Flush()
		},
	}
}

func restoreCmd() *cobra.Command {
	var diskOnly bool
	c := &cobra.Command{
		Use:   "restore <vm> <snapshot>",
		Short: "Return a VM to a snapshot",
		Long: `A memory snapshot resumes the VM as it was (starting it if stopped). A disk-only one
needs the VM off: a running VM is stopped and started again. Changes since the snapshot
are lost. Credentials are pushed again afterwards. --disk-only restores just the disk of
a memory snapshot (e.g. one saved by another QEMU version, which can't be loaded).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			return m.Restore(cmd.Context(), args[0], args[1], diskOnly)
		},
	}
	c.Flags().BoolVar(&diskOnly, "disk-only", false, "restore only the disk, booting afresh")
	return c
}

func rmsnapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rmsnap <vm> <snapshot>...",
		Short: "Delete snapshots",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			var errs []error
			for _, s := range args[1:] {
				errs = append(errs, m.DeleteSnapshot(args[0], s))
			}
			return errors.Join(errs...)
		},
	}
}

// confirmSnapshotLoss asks before an operation that deletes vm's snapshots.
func confirmSnapshotLoss(vm, op string, yes bool) error {
	m, err := manager()
	if err != nil {
		return err
	}
	ss, err := m.Snapshots(vm)
	if err != nil || len(ss) == 0 || yes {
		return err
	}
	var names []string
	for _, s := range ss {
		names = append(names, s.Name)
	}
	return confirm(fmt.Sprintf("%s deletes the snapshots of %s (%s). Continue?", op, vm, strings.Join(names, ", ")))
}

func resetCmd() *cobra.Command {
	var latest, yes bool
	c := &cobra.Command{
		Use:   "reset <vm>",
		Short: "Give a VM a fresh disk and run first boot again",
		Long: `Everything on the VM's disk is lost; its keys, forges and settings stay, and its repos
are cloned again. --latest uses the template's current base (new packages after
catchpen template rebuild). A running VM is started again afterwards.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				if err := confirm(fmt.Sprintf("Reset %s and delete all data on its disk, snapshots included?", args[0])); err != nil {
					return err
				}
			}
			m, err := manager()
			if err != nil {
				return err
			}
			return m.Reset(cmd.Context(), args[0], latest)
		},
	}
	c.Flags().BoolVar(&latest, "latest", false, "use the template's current base")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return c
}

func rebaseCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "rebase <vm>",
		Short: "Move a VM's disk onto the template's current base, keeping its contents",
		Long: `The guest sees the same disk afterwards (new packages need catchpen reset --latest);
the old base is then no longer used and catchpen gc can remove it. Snapshots are deleted.
A running VM is stopped and started again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := confirmSnapshotLoss(args[0], "Rebasing", yes); err != nil {
				return err
			}
			m, err := manager()
			if err != nil {
				return err
			}
			return m.Rebase(cmd.Context(), args[0])
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return c
}

func gcCmd() *cobra.Command {
	var dry bool
	c := &cobra.Command{
		Use:   "gc",
		Short: "Remove unused bases and leftovers of interrupted work",
		Long: `Removes bases no VM uses (each template's current base stays), and leftovers of
interrupted builds, downloads, resets and destroys, and trims the package caches to
cache_max. Recordings, transcripts, tasks and the audit log are kept.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			items, err := m.GC(dry)
			if err != nil {
				return err
			}
			var total int64
			for _, it := range items {
				total += it.Bytes
				rel, err := filepath.Rel(m.P.Data, it.Path)
				if err != nil || strings.HasPrefix(rel, "..") {
					rel = it.Path
				}
				fmt.Printf("%-9s %s (%s)\n", humanBytes(it.Bytes), rel, it.Why)
			}
			verb := "freed"
			if dry {
				verb = "would free"
			}
			fmt.Printf("%s %s\n", verb, humanBytes(total))
			return nil
		},
	}
	c.Flags().BoolVarP(&dry, "dry-run", "n", false, "only list what would be removed")
	return c
}
