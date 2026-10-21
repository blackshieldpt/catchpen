package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/audit"
	"github.com/blackshieldpt/curral/internal/canary"
	"github.com/blackshieldpt/curral/internal/egress"
	"github.com/blackshieldpt/curral/internal/notify"
	"github.com/blackshieldpt/curral/internal/vm"
)

func egressCmd() *cobra.Command {
	c := &cobra.Command{Use: "egress", Short: "Inspect and manage VM egress allowlists"}

	var follow bool
	logc := &cobra.Command{
		Use:   "log <name>",
		Short: "Show egress decisions (allowed and blocked requests)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if _, err := m.Load(args[0]); err != nil {
				return err
			}
			return egress.TailLog(cmd.Context(), filepath.Join(m.Dir(args[0]), egress.LogFile), follow, os.Stdout)
		},
	}
	logc.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries")

	edit := func(use, short, file string, add bool) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <name> <host[:port]>...",
			Short: short,
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				m, err := manager()
				if err != nil {
					return err
				}
				s, err := m.Load(args[0])
				if err != nil {
					return err
				}
				if s.Egress != egress.ModeAllowlist {
					return fmt.Errorf("VM %s uses egress mode %q; lists only apply to allowlist mode", s.Name, s.Egress)
				}
				for _, e := range args[1:] {
					if _, err := egress.ParseEntry(e); err != nil {
						return err
					}
					if add {
						err = egress.AddEntry(m.Dir(s.Name), file, e)
					} else {
						err = egress.RemoveEntry(m.Dir(s.Name), file, e)
					}
					if err != nil {
						return err
					}
					ev := audit.EgressAllow
					if file == egress.DenylistFile {
						ev = audit.EgressDeny
					}
					_ = m.Audit().Record(ev, s.Name, map[string]any{"entry": e, "add": add})
				}
				return nil
			},
		}
	}

	show := &cobra.Command{
		Use:   "show <name>",
		Short: "Print the VM's allowlist and denylist",
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
			fmt.Printf("egress: %s\n", s.Egress)
			if s.Egress != egress.ModeAllowlist {
				return nil
			}
			cfg, err := egress.ReadConfig(m.Dir(s.Name))
			if err != nil {
				return err
			}
			fmt.Printf("on_deny: %s (prompt timeout %s), allow_private: %v\n", cfg.OnDeny, cfg.PromptTimeout, cfg.AllowPrivate)
			for _, f := range []string{egress.AllowlistFile, egress.DenylistFile} {
				rules, err := egress.LoadList(filepath.Join(m.Dir(s.Name), f))
				if err != nil {
					return err
				}
				fmt.Printf("\n%s:\n", f)
				for _, r := range rules {
					fmt.Printf("  %s\n", r)
				}
			}
			return nil
		},
	}

	var onDeny, timeout string
	var allowPrivate bool
	cfgc := &cobra.Command{
		Use:   "config <name>",
		Short: "Change a VM's egress settings (on_deny, prompt timeout, private addresses)",
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
			if s.Egress != egress.ModeAllowlist {
				return fmt.Errorf("VM %s uses egress mode %q", s.Name, s.Egress)
			}
			c, err := egress.ReadConfig(m.Dir(s.Name))
			if err != nil {
				return err
			}
			f := cmd.Flags()
			if f.Changed("on-deny") {
				if onDeny != egress.OnDenyLog && onDeny != egress.OnDenyPrompt {
					return fmt.Errorf("--on-deny must be log or prompt")
				}
				c.OnDeny = onDeny
			}
			if f.Changed("prompt-timeout") {
				if c.PromptTimeout, err = time.ParseDuration(timeout); err != nil || c.PromptTimeout <= 0 {
					return fmt.Errorf("--prompt-timeout: invalid duration %q", timeout)
				}
			}
			if f.Changed("allow-private") {
				c.AllowPrivate = allowPrivate
			}
			if err := egress.WriteConfig(m.Dir(s.Name), c); err != nil {
				return err
			}
			fmt.Printf("on_deny: %s, prompt_timeout: %s, allow_private: %v\n", c.OnDeny, c.PromptTimeout, c.AllowPrivate)
			return nil
		},
	}
	cfgc.Flags().StringVar(&onDeny, "on-deny", "", "log | prompt")
	cfgc.Flags().StringVar(&timeout, "prompt-timeout", "", "how long a prompt waits before denying, e.g. 30s")
	cfgc.Flags().BoolVar(&allowPrivate, "allow-private", false, "allow allowlisted names that resolve to private/loopback addresses")

	var desktop bool
	watch := &cobra.Command{
		Use:   "watch [name...]",
		Short: "Answer egress prompts for VMs with on_deny: prompt (default: all VMs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				vms, err := m.List()
				if err != nil {
					return err
				}
				for _, v := range vms {
					if v.Egress == egress.ModeAllowlist && !v.Pool {
						names = append(names, v.Name)
					}
				}
			}
			dirs := map[string]string{}
			for _, n := range names {
				s, err := m.Load(n)
				if err != nil {
					return err
				}
				if s.Egress != egress.ModeAllowlist {
					return fmt.Errorf("VM %s uses egress mode %q", n, s.Egress)
				}
				if cfg, err := egress.ReadConfig(m.Dir(n)); err == nil && cfg.OnDeny != egress.OnDenyPrompt {
					fmt.Fprintf(os.Stderr, "note: %s has on_deny: %s; its proxy won't ask\n", n, cfg.OnDeny)
				}
				dirs[n] = m.Dir(n)
			}
			if len(dirs) == 0 {
				return errors.New("no VMs with allowlist egress")
			}
			decide := egress.TerminalDecider(os.Stdin, os.Stderr)
			if desktop {
				decide = egress.DesktopDecider()
			}
			decide = promptDecider(m, decide)
			fmt.Fprintf(os.Stderr, "watching %d VM(s); Ctrl-C to stop\n", len(dirs))
			err = egress.Watch(cmd.Context(), dirs, decide)
			if errors.Is(err, cmd.Context().Err()) {
				return nil
			}
			return err
		},
	}
	watch.Flags().BoolVar(&desktop, "desktop", false, "prompt with desktop notifications instead of the terminal")

	c.AddCommand(logc, show, cfgc, watch,
		edit("allow", "Add entries to a VM's allowlist (applies to new connections immediately)", egress.AllowlistFile, true),
		edit("unallow", "Remove entries from a VM's allowlist", egress.AllowlistFile, false),
		edit("deny", "Add entries to a VM's denylist (wins over the allowlist)", egress.DenylistFile, true),
		edit("undeny", "Remove entries from a VM's denylist", egress.DenylistFile, false),
	)
	return c
}

// promptDecider wraps a decider with the prompt notification and the
// audit record of the answer.
func promptDecider(m *vm.Manager, inner egress.Decider) egress.Decider {
	alog, nt := m.Audit(), m.Notifier()
	return func(ctx context.Context, r egress.Request) egress.Decision {
		if nt.Wants(notify.EgressPrompt) {
			go func() {
				if err := nt.Send(context.Background(), notify.Event{Kind: notify.EgressPrompt, VM: r.VM,
					Title: "curral " + r.VM + ": egress prompt", Message: fmt.Sprintf("%s %s:%d is waiting for an answer", r.Method, r.Host, r.Port)}); err != nil {
					fmt.Fprintf(os.Stderr, "warning: notification: %v\n", err)
				}
			}()
		}
		start := time.Now()
		d := inner(ctx, r)
		// Only answers given by a person: timeouts are in the VM's egress log, and
		// a guest could otherwise flood the audit log until it rotates. So could
		// a decider failing at once (notify-send without a notification server).
		if ctx.Err() == nil && time.Since(start) > 300*time.Millisecond {
			_ = alog.Record(audit.EgressPrompt, r.VM, map[string]any{"host": r.Host, "port": r.Port, "method": r.Method, "decision": string(d)})
		}
		return d
	}
}

// canaryHit hands a canary hit seen by a guest-facing process to the manager.
func canaryHit(name string) func(*canary.Canary, string) {
	return func(c *canary.Canary, where string) {
		if m, err := manager(); err == nil {
			m.CanaryHit(name, c, where)
		}
	}
}

func egressStdioCmd() *cobra.Command {
	var dir, name string
	c := &cobra.Command{
		Use:    "egress-stdio",
		Short:  "Serve one guest proxy connection on stdin/stdout (spawned by QEMU)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// stderr may be the guest socket too; never write diagnostics into the guest.
			// If that fails, exit silently rather than risk it.
			devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				os.Exit(1)
			}
			if err := syscall.Dup3(int(devnull.Fd()), 2, 0); err != nil {
				os.Exit(1)
			}
			devnull.Close()
			conn, err := net.FileConn(os.Stdin)
			if err != nil {
				return err
			}
			// stdin and stdout are the same socket; drop our copies so only conn holds it.
			os.Stdin.Close()
			os.Stdout.Close()
			p := &egress.Proxy{Dir: dir, VM: name, OnCanary: canaryHit(name)}
			return p.ServeConn(cmd.Context(), conn)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "VM directory")
	c.Flags().StringVar(&name, "vm", "", "VM name")
	_ = c.MarkFlagRequired("dir")
	_ = c.MarkFlagRequired("vm")
	return c
}
