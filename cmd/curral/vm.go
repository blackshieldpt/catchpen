package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/sshx"
	"github.com/blackshieldpt/curral/internal/vm"
)

func upCmd() *cobra.Command {
	var o vm.CreateOpts
	c := &cobra.Command{
		Use:   "up <name>",
		Short: "Create (if needed) and start a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := args[0]
			if _, err := m.Load(name); err != nil {
				if _, statErr := os.Stat(m.Dir(name)); statErr == nil {
					return err
				}
				if _, err := m.Create(cmd.Context(), name, o); err != nil {
					return err
				}
			} else {
				for _, fl := range []string{"template", "distro", "cpus", "mem", "disk", "rebuild", "allow-open", "claude-auth", "repo", "ro", "ssh-agent", "agent-key"} {
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
	f.StringVarP(&o.Template, "template", "t", "", "template (default from config)")
	f.StringVar(&o.Distro, "distro", "", "guest distro (overrides template/config)")
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
	return c
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
				fmt.Fprintf(os.Stderr, "Destroy %s and delete all data inside? [y/N] ", strings.Join(args, ", "))
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
					return errors.New("aborted")
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
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tTEMPLATE\tDISTRO\tCPU\tMEM\tEGRESS\tSSH\tPORTS\tAGE")
			for _, v := range vms {
				state, ssh, ports := "stopped", "-", "-"
				if v.Running {
					state = "running"
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

func sshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <name> [-- command...]",
		Short: "Open a shell (or run a command) in a VM",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if _, err := m.SSHPort(args[0]); err != nil {
				return err
			}
			sshArgs := []string{"-F", m.P.SSHConfig()}
			if len(args) == 1 {
				sshArgs = append(sshArgs, "-t")
			}
			sshArgs = append(sshArgs, sshx.AliasPrefix+args[0])
			sshArgs = append(sshArgs, args[1:]...)
			c := exec.CommandContext(cmd.Context(), "ssh", sshArgs...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					return exitError(ee.ExitCode())
				}
				return err
			}
			return nil
		},
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

func templateCmd() *cobra.Command {
	c := &cobra.Command{Use: "template", Short: "Manage templates"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List templates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			all, err := m.Tpl.List()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(all))
			for n := range all {
				names = append(names, n)
			}
			sort.Strings(names)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSOURCE\tDESCRIPTION")
			for _, n := range names {
				desc := ""
				if t, err := m.Tpl.Load(n); err == nil {
					desc = t.Description
				} else {
					desc = "error: " + err.Error()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", n, all[n], desc)
			}
			return w.Flush()
		},
	}
	show := &cobra.Command{
		Use:   "show <template>",
		Short: "Print a template with its extends chain resolved",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			r, err := m.Resolve(args[0], "")
			if err != nil {
				return err
			}
			b, err := r.T.YAML()
			if err != nil {
				return err
			}
			fmt.Printf("# source: %s\n# base: %s\n%s", r.T.Source, r.BasePath(m.P), b)
			return nil
		},
	}
	var distro string
	var rebuild bool
	build := &cobra.Command{
		Use:   "build <template>",
		Short: "Build (or rebuild) a template base image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			r, err := m.Resolve(args[0], distro)
			if err != nil {
				return err
			}
			_, err = m.EnsureBase(cmd.Context(), r, rebuild)
			return err
		},
	}
	build.Flags().StringVar(&distro, "distro", "", "guest distro")
	build.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild even if the base exists")
	c.AddCommand(ls, show, build)
	return c
}
