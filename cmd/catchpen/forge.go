package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/forge"
	"github.com/blackshieldpt/catchpen/internal/vm"
)

func forgeCmd() *cobra.Command {
	c := &cobra.Command{Use: "forge", Short: "GitHub and Gitea forges VMs clone from"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List configured forges",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			reg, err := forge.Load(m.P.Forges())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tHOST\tAUTH\tHOST KEYS")
			for _, f := range reg.Forges {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", f.Name, f.Type, f.WebURL(), f.Auth.Mode, len(f.HostKeys))
			}
			return tw.Flush()
		},
	}

	var f forge.Forge
	var tokenSecret, passwordSecret, scopes string
	var yes bool
	add := &cobra.Command{
		Use:   "add <name> --type github|gitea --host H --mode deploy-key|minted-token|token|agent|none",
		Short: "Add a forge, pinning its SSH host key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			f.Name = args[0]
			if tokenSecret != "" {
				f.Auth.Token = "secret:" + tokenSecret
			}
			if passwordSecret != "" {
				f.Auth.Password = "secret:" + passwordSecret
			}
			if scopes != "" {
				f.Auth.Scopes = strings.Split(scopes, ",")
			}
			if reg, err := forge.Load(m.P.Forges()); err != nil {
				return err
			} else if err := reg.CheckNew(f.Name); err != nil {
				return err
			}
			keys, err := pinHostKeys(cmd.Context(), f, yes)
			if err != nil {
				if f.UsesSSH() {
					return err
				}
				fmt.Fprintf(os.Stderr, "warning: %v (only needed for deploy-key and agent modes)\n", err)
			}
			f.HostKeys = keys
			if err := forge.Add(cmd.Context(), m.P.Forges(), f); err != nil {
				return err
			}
			warnCleartext(f)
			fmt.Printf("added forge %s (%s); check credentials with: catchpen forge test %s\n", f.Name, f.WebURL(), f.Name)
			return nil
		},
	}
	fl := add.Flags()
	fl.StringVar(&f.Type, "type", "", "github or gitea")
	fl.StringVar(&f.Host, "host", "", "forge host name, e.g. github.com")
	fl.StringVar(&f.HTTPURL, "http-url", "", "web/API base URL (default https://<host>)")
	fl.IntVar(&f.SSHPort, "ssh-port", 0, "SSH port (default 22)")
	fl.StringVar(&f.Auth.Mode, "mode", "", "deploy-key | minted-token | token | agent | none")
	fl.StringVar(&tokenSecret, "token-secret", "", "secret name holding the API token (deploy-key, token)")
	fl.StringVar(&f.Auth.User, "user", "", "Gitea account to mint tokens for (minted-token)")
	fl.StringVar(&passwordSecret, "password-secret", "", "secret name holding that account's password (minted-token)")
	fl.StringVar(&scopes, "scopes", "write:repository,read:user", "minted token scopes, comma-separated")
	fl.BoolVarP(&yes, "yes", "y", false, "trust the scanned host key without asking")
	_ = add.MarkFlagRequired("type")
	_ = add.MarkFlagRequired("host")
	_ = add.MarkFlagRequired("mode")

	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a forge (VMs keep their snapshot of it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			return forge.Update(m.P.Forges(), func(r *forge.Registry) error {
				for i, f := range r.Forges {
					if f.Name == args[0] {
						r.Forges = append(r.Forges[:i], r.Forges[i+1:]...)
						return nil
					}
				}
				return fmt.Errorf("no forge %q", args[0])
			})
		},
	}

	test := &cobra.Command{
		Use:   "test <name>",
		Short: "Check a forge's credentials and host key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			reg, err := forge.Load(m.P.Forges())
			if err != nil {
				return err
			}
			f, ok := reg.Get(args[0])
			if !ok {
				return fmt.Errorf("no forge %q", args[0])
			}
			return testForge(cmd.Context(), m, f)
		},
	}
	c.AddCommand(ls, add, rm, test)
	return c
}

func pinHostKeys(ctx context.Context, f forge.Forge, yes bool) ([]string, error) {
	scan, err := (&forge.Client{F: f}).ScanHostKeys(ctx)
	if err != nil {
		return nil, err
	}
	if scan.Published {
		fmt.Printf("pinned %d GitHub host keys from api.github.com/meta\n", len(scan.Keys))
		return scan.Keys, nil
	}
	fmt.Printf("SSH host keys of %s port %d:\n", f.Host, f.Port())
	for _, fp := range scan.Fingerprints {
		fmt.Printf("  %s\n", fp)
	}
	if !yes {
		fmt.Print("Do these match the forge's published fingerprints? Trust them? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, fmt.Errorf("host keys not trusted")
		}
	}
	return scan.Keys, nil
}

// warnCleartext says when an http:// http_url sends a forge's credentials unencrypted.
func warnCleartext(f forge.Forge) {
	if f.CleartextCredentials() {
		fmt.Fprintf(os.Stderr, "warning: forge %s uses an http:// URL, so its token or password crosses the network unencrypted; use https\n", f.Name)
	}
}

func testForge(ctx context.Context, m *vm.Manager, f forge.Forge) error {
	warnCleartext(f)
	r, err := m.TestForge(ctx, f)
	if r.API {
		fmt.Printf("API ok: authenticated as %s\n", r.Login)
	}
	if r.HostKeys {
		fmt.Println("SSH host keys match")
	}
	if r.HostBound {
		fmt.Println("SSH server supports host-bound auth (agent mode works)")
	}
	return err
}

func pullCmd() *cobra.Command {
	var o vm.PullOpts
	c := &cobra.Command{
		Use:   "pull <name>",
		Short: "Fetch a branch from a VM's repo into refs/catchpen/<vm>/<branch> of a host repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			p, err := m.Pull(cmd.Context(), args[0], o)
			if err != nil {
				return err
			}
			fmt.Printf("%s\n  git log %s\n", p.Ref, p.Ref)
			return nil
		},
	}
	pullFlags(c, &o)
	return c
}

func pullFlags(c *cobra.Command, o *vm.PullOpts) {
	f := c.Flags()
	f.StringVar(&o.Repo, "repo", "", "repo in the VM (name or forge:owner/repo; default: the only one)")
	f.StringVarP(&o.Branch, "branch", "b", "", "branch (default: the clone's current branch)")
	f.StringVar(&o.Into, "into", ".", "host git repository to fetch into")
	f.StringVar(&o.Patches, "patches", "", "also write the branch as an mbox of patches into this directory")
}

func prCmd() *cobra.Command {
	var o vm.PROpts
	c := &cobra.Command{
		Use:   "pr <name>",
		Short: "Pull a branch, push it with your host git credentials and open a pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			url, err := m.PR(cmd.Context(), args[0], o)
			if err != nil {
				return err
			}
			fmt.Println(url)
			return nil
		},
	}
	pullFlags(c, &o.PullOpts)
	f := c.Flags()
	f.StringVar(&o.Title, "title", "", "PR title (default: last commit subject)")
	f.StringVar(&o.BodyFile, "body-file", "", "PR description file")
	f.BoolVar(&o.Draft, "draft", false, "open as draft (Gitea: WIP title)")
	f.StringVar(&o.Base, "base", "", "target branch (default: the repo's default branch)")
	f.StringVar(&o.Head, "head", "", "branch name on the forge (default catchpen/<vm>/<branch>)")
	return c
}
