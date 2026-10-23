package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/record"
	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
	"github.com/blackshieldpt/curral/internal/vm"
)

func exitCode(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func sshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <name> [-- command...]",
		Short: "Open a shell (or run a command) in a VM; interactive shells are recorded",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if len(args) > 1 {
				// A command: no tty, not recorded (ssh joins the words, as here).
				return exitCode(m.Session(cmd.Context(), args[0], vm.KindCommand, strings.Join(args[1:], " ")))
			}
			return exitCode(m.Session(cmd.Context(), args[0], vm.KindSSH, ""))
		},
	}
}

func runCmd() *cobra.Command {
	var yolo []string
	for _, d := range template.AgentDefs {
		if d.Yolo != "" {
			yolo = append(yolo, d.Name)
		}
	}
	return &cobra.Command{
		Use:   "run <name> " + strings.Join(template.AgentNames(), "|") + " [-- agent args...]",
		Short: "Start (or reattach to) an agent in a guest tmux session",
		Long: `The agent runs in a guest tmux session named after it, in the VM's repo when it has
exactly one (else /workspace), so it survives disconnects. Guest tmux prefix: C-a.
Template agents.<name>.yolo: true starts ` + strings.Join(yolo, "/") + ` without permission prompts.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			s, err := m.Load(args[0])
			if err != nil {
				return err
			}
			remote, err := vm.RunCommand(s, args[1], args[2:])
			if err != nil {
				return err
			}
			return exitCode(m.Session(cmd.Context(), s.Name, vm.KindRun, remote))
		},
	}
}

func attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <name>",
		Short: "Attach to the VM's main guest tmux session (created if missing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			s, err := m.Load(args[0])
			if err != nil {
				return err
			}
			return exitCode(m.Session(cmd.Context(), s.Name, vm.KindAttach, vm.AttachCommand(s)))
		},
	}
}

func recordingsCmd() *cobra.Command {
	var speed float64
	var idle time.Duration
	c := &cobra.Command{
		Use:   "recordings <name> [play <id>]",
		Short: "List or replay a VM's host-side session recordings (asciicast v2)",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := args[0]
			if len(args) > 1 {
				if args[1] != "play" || len(args) != 3 {
					return errors.New("usage: curral recordings <name> play <id>")
				}
				ids, err := m.Recordings(name)
				if err != nil {
					return err
				}
				if !contains(ids, args[2]) {
					return fmt.Errorf("no recording %q for %s (see: curral recordings %s)", args[2], name, name)
				}
				f, err := os.Open(m.RecordingPath(name, args[2]))
				if err != nil {
					return err
				}
				defer f.Close()
				// Guest output: drop sequences that reach beyond the screen (title, clipboard).
				return record.Play(f, &termFilter{w: os.Stdout}, speed, idle)
			}
			ids, err := m.Recordings(name)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTARTED\tDURATION\tSIZE\tPATH")
			for _, id := range ids {
				p := m.RecordingPath(name, id)
				in, err := record.Stat(p)
				if err != nil {
					fmt.Fprintf(tw, "%s\t-\t-\t-\t%s\n", id, p)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, time.Unix(in.Timestamp, 0).Format("2006-01-02 15:04"),
					in.Duration.Round(time.Second), humanBytes(in.Size), p)
			}
			return tw.Flush()
		},
	}
	c.Flags().Float64Var(&speed, "speed", 1, "playback speed")
	c.Flags().DurationVar(&idle, "idle", 2*time.Second, "cap pauses at this long")
	return c
}

func auditCmd() *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use:   "audit [<name>]",
		Short: "Show the host audit log (all VMs, or one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				if err := vm.ValidName(args[0]); err != nil {
					return err
				}
				name = args[0]
			}
			return m.Audit().Tail(cmd.Context(), name, follow, os.Stdout)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events")
	return c
}

func tmuxCmd() *cobra.Command {
	var all, sync bool
	c := &cobra.Command{
		Use:   "tmux [<name>...|--all] | tmux --sync",
		Short: "Open a host tmux session with one window per VM (each attached to its guest tmux)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			running, err := runningVMs(m)
			if err != nil {
				return err
			}
			if sync {
				if len(args) > 0 || all {
					return errors.New("--sync takes no VM names")
				}
				return m.Tmux.Sync(running)
			}
			names := args
			if all || len(names) == 0 {
				names = running
			}
			for _, n := range names {
				if !contains(running, n) {
					return fmt.Errorf("%s is not running (curral start %s)", n, n)
				}
			}
			if len(names) == 0 && !m.Tmux.Exists() {
				return errors.New("no running VMs")
			}
			if len(names) > 0 {
				if err := m.Tmux.Ensure(names); err != nil {
					return err
				}
			}
			first := ""
			if len(args) > 0 {
				first = args[0]
			}
			return m.Tmux.Attach(first)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "a window for every running VM (the default with no names)")
	c.Flags().BoolVar(&sync, "sync", false, "add windows for running VMs and close curral windows of stopped ones, without attaching")
	return c
}

func runningVMs(m *vm.Manager) ([]string, error) {
	vms, err := m.List()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vms {
		if v.Running {
			out = append(out, v.Name)
		}
	}
	return out, nil
}

func cpCmd() *cobra.Command {
	var recursive bool
	c := &cobra.Command{
		Use:   "cp [-r] <src>... <dst>",
		Short: "Copy files to or from VMs (vm:path) with scp",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			var paths []string
			var remote []string
			for _, a := range args {
				name, p, ok := strings.Cut(a, ":")
				if !ok || strings.Contains(name, "/") || vm.ValidName(name) != nil {
					paths = append(paths, a)
					continue
				}
				if _, err := m.SSHPort(name); err != nil {
					return err
				}
				remote = append(remote, name)
				paths = append(paths, sshx.AliasPrefix+name+":"+p)
			}
			if len(remote) == 0 {
				return errors.New("no VM path (use <vm>:<path>)")
			}
			scp := []string{"-F", m.P.SSHConfig(), "-q"}
			if recursive {
				scp = append(scp, "-r")
			}
			c := exec.CommandContext(cmd.Context(), "scp", append(append(scp, "--"), paths...)...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			err = c.Run()
			for _, n := range uniq(remote) {
				_ = m.Audit().Record(audit.Copy, n, map[string]any{"args": args, "ok": err == nil})
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return exitError(ee.ExitCode())
			}
			return err
		},
	}
	c.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy directories")
	return c
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func uniq(l []string) []string {
	var out []string
	for _, x := range l {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
