package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/template"
	"github.com/blackshieldpt/curral/internal/vm"
)

func upCmd() *cobra.Command {
	var o vm.CreateOpts
	c := &cobra.Command{
		Use:   "up <name> [--ephemeral [-- command...]]",
		Short: "Create (if needed) and start a VM",
		Long: `--ephemeral creates a new VM, attaches to it (or runs the command), and destroys it,
with its deploy keys and tokens, when the session ends or curral is signalled.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := args[0]
			if o.Ephemeral {
				return upEphemeral(cmd, m, name, o, args[1:])
			}
			if len(args) > 1 {
				return errors.New("a command after the name needs --ephemeral (else use curral ssh)")
			}
			if _, err := m.Load(name); err != nil {
				if _, statErr := os.Stat(m.Dir(name)); statErr == nil {
					return err
				}
				if _, err := m.Create(cmd.Context(), name, o); err != nil {
					return err
				}
			} else {
				for _, fl := range []string{"template", "cpus", "mem", "disk", "rebuild", "allow-open", "claude-auth", "repo", "ro", "ssh-agent", "agent-key"} {
					if cmd.Flags().Changed(fl) {
						return fmt.Errorf("VM %s already exists; --%s only applies on creation", name, fl)
					}
				}
			}
			_, err = m.Start(cmd.Context(), name)
			return err
		},
	}
	f := c.Flags()
	f.StringVarP(&o.Template, "template", "t", "", templateFlagHelp)
	f.IntVar(&o.CPUs, "cpus", 0, "vCPUs")
	f.StringVar(&o.Memory, "mem", "", "memory, e.g. 8G")
	f.StringVar(&o.Disk, "disk", "", "disk size, e.g. 40G")
	f.BoolVar(&o.Rebuild, "rebuild", false, "rebuild the template base first")
	f.BoolVar(&o.AllowOpen, "allow-open", false, "confirm a template with network.egress: open")
	f.StringArrayVar(&o.Repos, "repo", nil, "clone a repo into /workspace (forge:owner/repo or URL; repeatable)")
	f.BoolVar(&o.ReadOnly, "ro", false, "make deploy keys read-only")
	f.StringVar(&o.SSHAgent, "ssh-agent", "", "agent forwarding: off | restricted | host (default from template)")
	f.StringArrayVar(&o.AgentKeys, "agent-key", nil, "host key file for the restricted agent (repeatable)")
	f.StringVar(&o.ClaudeAuth, "claude-auth", "", "Claude Code auth for this VM: oauth-token | api-key (default from template)")
	f.BoolVar(&o.Ephemeral, "ephemeral", false, "destroy the VM when the session ends")
	return c
}

func upEphemeral(cmd *cobra.Command, m *vm.Manager, name string, o vm.CreateOpts, command []string) error {
	if _, err := os.Stat(m.Dir(name)); err == nil {
		return fmt.Errorf("VM %s exists; --ephemeral needs a new name", name)
	}
	ctx := cmd.Context()
	created := false // Create cleans up after itself; never destroy a VM it didn't make
	defer func() {
		if !created {
			return
		}
		if err := m.Destroy(name); err != nil {
			fmt.Fprintf(os.Stderr, "curral: destroying ephemeral %s: %v (retry: curral destroy -y %s)\n", name, err, name)
		}
	}()
	s, err := m.Create(ctx, name, o)
	if err != nil {
		return err
	}
	created = true
	if _, err := m.Start(ctx, name); err != nil {
		return err
	}
	if len(command) > 0 {
		q := make([]string, len(command))
		for i, a := range command {
			q[i] = shellQuote(a)
		}
		remote := strings.Join(q, " ")
		if d := vm.WorkDir(s); d != "" {
			remote = "cd " + shellQuote(d) + " && " + remote
		}
		return exitCode(m.Session(ctx, name, vm.KindSSH, remote))
	}
	return exitCode(m.Session(ctx, name, vm.KindAttach, vm.AttachCommand(s)))
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start an existing VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			_, err = m.Start(cmd.Context(), args[0])
			return err
		},
	}
}

func stopCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "stop <name>...",
		Short: "Shut down VMs (ACPI power-off, then force after a timeout)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			var errs []error
			for _, n := range args {
				errs = append(errs, m.Stop(n, force))
			}
			return errors.Join(errs...)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "power off immediately")
	return c
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Continue a VM a canary hit paused",
		Long: `A VM whose decoy credentials (template canaries:) were used is paused, frozen as it
was (config.yaml canary_action). Look at what it did first (curral audit, curral egress
log), then resume it, or stop or destroy it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			return m.Resume(cmd.Context(), args[0])
		},
	}
}

func destroyCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "destroy <name>...",
		Short: "Stop and delete VMs, including their disks",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if !yes {
				if err := confirm(fmt.Sprintf("Destroy %s and delete all data inside?", strings.Join(args, ", "))); err != nil {
					return err
				}
			}
			var errs []error
			for _, n := range args {
				errs = append(errs, m.Destroy(n))
			}
			return errors.Join(errs...)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return c
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			vms, err := m.List()
			if err != nil {
				return err
			}
			current := map[string]string{} // template -> current base
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tTEMPLATE\tDISTRO\tCPU\tMEM\tEGRESS\tSSH\tPORTS\tAGE")
			for _, v := range vms {
				if v.Pool { // curral pool ls
					continue
				}
				state, ssh, ports := "stopped", "-", "-"
				if v.Running {
					state = "running"
				}
				if v.Paused {
					state = "paused" // canary hit
				}
				if v.Ephemeral {
					state += ",ephemeral"
				}
				if v.Task != "" {
					state += ",task"
				}
				if _, ok := current[v.Template]; !ok && v.Picks {
					current[v.Template], _ = m.CurrentBase(v.Template)
				}
				if cur := current[v.Template]; cur != "" && cur != v.Base {
					state += ",stale-base"
				}
				if v.Running {
					if v.Runtime != nil && v.Runtime.SSHPort != 0 {
						ssh = fmt.Sprintf("127.0.0.1:%d", v.Runtime.SSHPort)
						var ps []string
						for _, f := range v.Runtime.Forwards {
							ps = append(ps, fmt.Sprintf("%d->%d", f.Host, f.Guest))
						}
						if len(ps) > 0 {
							ports = strings.Join(ps, ",")
						}
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%dM\t%s\t%s\t%s\t%s\n", v.Name, state, v.Template, v.Distro,
					v.CPUs, v.MemoryMB, v.Egress, ssh, ports, age(v.Created))
			}
			return w.Flush()
		},
	}
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func sshProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "ssh-proxy <name>",
		Short:  "ssh ProxyCommand helper",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			port, err := m.SSHPort(args[0])
			if err != nil {
				return err
			}
			return sshx.Proxy(port, os.Stdin, os.Stdout)
		},
	}
}

func setupSSHCmd() *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:   "setup-ssh",
		Short: "Include curral's ssh_config in ~/.ssh/config (enables `ssh curral-<name>`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if err := m.WriteSSHConfig(); err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			userCfg := home + "/.ssh/config"
			changed, err := sshx.SetupInclude(userCfg, m.P.SSHConfig(), remove)
			if err != nil {
				return err
			}
			switch {
			case !changed:
				fmt.Println("nothing to do")
			case remove:
				fmt.Printf("removed curral Include from %s\n", userCfg)
			default:
				fmt.Printf("added Include %s to %s\n", m.P.SSHConfig(), userCfg)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "remove the Include line")
	return c
}

// templateFlagHelp describes -t on every command that takes it.
const templateFlagHelp = "picks from the category files, comma separated, e.g. go-1.27,large or -docker (curral template ls; default: every category's default)"

func templateCmd() *cobra.Command {
	c := &cobra.Command{Use: "template", Short: "List and build templates: picks from the category files"}
	ls := &cobra.Command{
		Use:   "ls [category]",
		Short: "List the sections of each category, marking the defaults",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			lib, err := m.Tpl.Library()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CATEGORY\tSECTION\tDEFAULT\tDESCRIPTION\tSOURCE")
			found := false
			for _, cat := range template.Categories {
				if len(args) > 0 && args[0] != cat.Name {
					continue
				}
				found = true
				secs, _ := lib.Sections(cat.Name)
				def, err := lib.Default(cat.Name)
				if err != nil {
					return err
				}
				for _, s := range def {
					if s.Name == "default" { // a default with settings of its own
						fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", cat.Name, "default", "yes", s.Description, s.Source)
					}
				}
				for _, s := range secs {
					d := ""
					if slices.Contains(def, s) {
						d = "yes"
					}
					desc := s.Description
					if s.Group != nil {
						desc = "[" + strings.Join(s.Group, ", ") + "]"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", cat.Name, s.Name, d, desc, s.Source)
				}
			}
			if !found {
				return fmt.Errorf("no category %q", args[0])
			}
			return w.Flush()
		},
	}
	show := &cobra.Command{
		Use:   "show [-t picks]",
		Short: "Print the settings a -t spec gives, and the sections they come from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			r, err := m.Resolve(specArg(cmd, args))
			if err != nil {
				return err
			}
			b, err := r.T.YAML()
			if err != nil {
				return err
			}
			for _, p := range r.T.Parts {
				d := ""
				if p.Default {
					d = " (default)"
				}
				fmt.Printf("# %-9s %s%s\n", p.Category+":", strings.Join(p.Sections, ", "), d)
			}
			fmt.Printf("# base: %s\n%s", r.BasePath(m.P), b)
			return nil
		},
	}
	var rebuild bool
	build := &cobra.Command{
		Use:   "build [-t picks]",
		Short: "Build (or rebuild) the base image for a -t spec",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			r, err := m.Resolve(specArg(cmd, args))
			if err != nil {
				return err
			}
			_, err = m.EnsureBase(cmd.Context(), r, rebuild)
			return err
		},
	}
	build.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild even if the base exists")
	var all bool
	rb := &cobra.Command{
		Use:   "rebuild [-t picks]... [--all]",
		Short: "Build a new base generation with a fresh cloud image and current packages",
		Long: `The cloud image is re-checked upstream (and re-downloaded if its checksum changed) and
a new base generation is built. Existing VMs keep their base (curral ls marks them stale):
curral reset --latest moves one to the new base, curral gc removes unused old bases.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			specs, _ := cmd.Flags().GetStringArray("template")
			specs = append(specs, args...)
			var targets []string
			switch {
			case all && len(specs) > 0:
				return errors.New("--all takes no -t")
			case all:
				if targets, err = m.RebuildTargets(); err != nil {
					return err
				}
			case len(specs) == 0:
				return errors.New("give -t picks (-t default for the defaults) or --all")
			}
			for _, sp := range specs {
				targets = append(targets, template.NormalizeSpec(sp))
			}
			return m.Rebuild(cmd.Context(), targets)
		},
	}
	rb.Flags().BoolVar(&all, "all", false, "every template with a base or a VM")
	rb.Flags().StringArrayP("template", "t", nil, templateFlagHelp+"; repeatable")
	for _, sc := range []*cobra.Command{show, build} {
		sc.Flags().StringP("template", "t", "", templateFlagHelp)
	}
	c.AddCommand(ls, show, build, rb)
	return c
}

// specArg is the -t flag, or the positional spec.
func specArg(cmd *cobra.Command, args []string) string {
	t, _ := cmd.Flags().GetString("template")
	if len(args) > 0 {
		t = args[0]
	}
	return t
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
