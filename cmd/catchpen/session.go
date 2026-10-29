package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/record"
	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
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

// termReset undoes what replayed guest output may have left set: colours or
// hidden text, a line-drawing character set, a hidden cursor, the alternate
// screen. Its leading ESC also ends a sequence the guest left unfinished.
const termReset = "\x1b[0m\x1b(B\x0f\x1b[?25h\x1b[?1047l"

// playRecording replays r on out through the terminal filter, then says on
// errw whether the recording is complete. That line is the host's, written
// after the replay and outside the filter, so the guest can't hide or fake
// it (a string it left open would swallow anything sent through the filter).
func playRecording(r io.Reader, out, errw io.Writer, tty bool, speed float64, idle time.Duration) error {
	fw := &termFilter{w: out}
	truncated, err := record.Play(r, fw, speed, idle)
	if tty {
		_, _ = io.WriteString(out, termReset)
	}
	if err != nil {
		return err
	}
	if truncated {
		fmt.Fprintln(errw, "catchpen: the recording stops here: it reached its size limit; the rest of the session was not recorded")
	} else {
		fmt.Fprintln(errw, "catchpen: end of recording")
	}
	return nil
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
					return errors.New("usage: catchpen recordings <name> play <id>")
				}
				ids, err := m.Recordings(name)
				if err != nil {
					return err
				}
				if !contains(ids, args[2]) {
					return fmt.Errorf("no recording %q for %s (see: catchpen recordings %s)", args[2], name, name)
				}
				f, err := os.Open(m.RecordingPath(name, args[2]))
				if err != nil {
					return err
				}
				defer f.Close()
				// Guest output: drop sequences that reach beyond the screen (title, clipboard).
				return playRecording(f, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdout.Fd())), speed, idle)
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
				dur := in.Duration.Round(time.Second).String()
				if in.Truncated {
					dur += " (truncated)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, time.Unix(in.Timestamp, 0).Format("2006-01-02 15:04"),
					dur, humanBytes(in.Size), p)
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
			vms, err := loadRunning(m)
			if err != nil {
				return err
			}
			names, err := tmuxTargets(vms, args, all, sync)
			if err != nil {
				return err
			}
			if sync {
				return m.Tmux.Sync(names)
			}
			if len(names) == 0 && !m.Tmux.Exists() {
				return noVMs(vms)
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
	c.Flags().BoolVar(&sync, "sync", false, "add windows for running VMs and close catchpen windows of stopped ones, without attaching")
	return c
}

func herdrCmd() *cobra.Command {
	var all, sync bool
	var agent string
	c := &cobra.Command{
		Use:   "herdr [<name>...|--all] [--agent A] | herdr --sync",
		Short: "Open herdr with one workspace per VM, each running an agent herdr tracks and resumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if agent != "" {
				if _, ok := template.AgentDefByName(agent); !ok {
					return fmt.Errorf("unknown agent %q (%s)", agent, strings.Join(template.AgentNames(), ", "))
				}
			}
			vms, err := loadRunning(m)
			if err != nil {
				return err
			}
			names, agentOf, notes, err := herdrTargets(vms, args, all, sync, agent)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintln(os.Stderr, "note: "+n)
			}
			of := func(vm string) string { return agentOf[vm] }
			if err := m.Herdr.StartServer(); err != nil {
				return err
			}
			if sync {
				return m.Herdr.Sync(names, of)
			}
			if err := m.Herdr.Ensure(names, of); err != nil {
				return err
			}
			first := ""
			if len(args) > 0 {
				first = args[0]
			}
			return m.Herdr.Attach(first)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "a workspace for every running VM (the default with no names)")
	c.Flags().BoolVar(&sync, "sync", false, "add workspaces for running VMs and close catchpen workspaces of stopped ones, without attaching")
	c.Flags().StringVar(&agent, "agent", "", "agent to run in new workspaces (default: claude if installed, else the VM's first agent)")
	return c
}

// runningVM is what catchpen tmux and herdr need to know about a running VM.
type runningVM struct {
	Name    string
	Task    bool     // runs a headless task: no window or workspace
	Default string   // the agent its herdr workspace runs ("" for a shell)
	Agents  []string // the agents installed in it
}

func loadRunning(m *vm.Manager) ([]runningVM, error) {
	running, err := runningVMs(m)
	if err != nil {
		return nil, err
	}
	var vms []runningVM
	for _, n := range running {
		s, err := m.Load(n)
		if err != nil {
			return nil, err
		}
		v := runningVM{Name: n, Task: s.Task != "", Default: s.DefaultAgent()}
		for _, a := range template.AgentNames() {
			if s.Agents.Get(a) != nil {
				v.Agents = append(v.Agents, a)
			}
		}
		vms = append(vms, v)
	}
	return vms, nil
}

// pickVMs checks the VM names given to catchpen tmux or herdr. With none, or
// all, it picks every running VM except task VMs: their agent runs headless
// and unattended, and an attached session would forward the VM's SSH agent.
func pickVMs(vms []runningVM, args []string, all bool) ([]string, error) {
	byName := map[string]runningVM{}
	var interactive []string
	for _, v := range vms {
		byName[v.Name] = v
		if !v.Task {
			interactive = append(interactive, v.Name)
		}
	}
	if all || len(args) == 0 {
		return interactive, nil
	}
	for _, n := range args {
		v, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("%s is not running (catchpen start %s)", n, n)
		}
		if v.Task {
			return nil, fmt.Errorf("%s runs a headless task (catchpen task logs)", n)
		}
	}
	return args, nil
}

// noVMs is the error when pickVMs picked nothing.
func noVMs(vms []runningVM) error {
	if len(vms) > 0 {
		return errors.New("only headless task VMs are running (catchpen task logs)")
	}
	return errors.New("no running VMs")
}

// tmuxTargets checks catchpen tmux's arguments and returns its VMs.
func tmuxTargets(vms []runningVM, args []string, all, sync bool) ([]string, error) {
	if sync && (len(args) > 0 || all) {
		return nil, errors.New("--sync takes no VM names")
	}
	return pickVMs(vms, args, all || sync)
}

// herdrTargets checks catchpen herdr's arguments, before anything starts,
// and returns its VMs, the agent for each, and a note for each VM that
// keeps its default because it lacks --agent.
func herdrTargets(vms []runningVM, args []string, all, sync bool, agent string) ([]string, map[string]string, []string, error) {
	if sync && (len(args) > 0 || all || agent != "") {
		return nil, nil, nil, errors.New("--sync takes no VM names or --agent")
	}
	names, err := pickVMs(vms, args, all || sync)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(names) == 0 && !sync {
		return nil, nil, nil, noVMs(vms)
	}
	byName := map[string]runningVM{}
	for _, v := range vms {
		byName[v.Name] = v
	}
	agentOf := map[string]string{}
	var notes []string
	for _, n := range names {
		v := byName[n]
		agentOf[n] = v.Default
		switch {
		case agent == "":
		case contains(v.Agents, agent):
			agentOf[n] = agent
		case contains(args, n):
			return nil, nil, nil, fmt.Errorf("%s is not installed in %s (create a VM whose -t has it, e.g. -t %s)", agent, n, agent)
		default:
			notes = append(notes, fmt.Sprintf("%s has no %s; if it gets a new workspace, that runs %s", n, agent, cmp.Or(v.Default, "a shell")))
		}
	}
	return names, agentOf, notes, nil
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
