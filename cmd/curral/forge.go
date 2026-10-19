package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/creds"
	"github.com/blackshieldpt/curral/internal/forge"
	"github.com/blackshieldpt/curral/internal/vm"
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
		Use:   "add <name> --type github|gitea --host H --mode deploy-key|minted-token|token|none",
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
			} else if _, ok := reg.Get(f.Name); ok {
				return fmt.Errorf("forge %s exists (curral forge rm %s first)", f.Name, f.Name)
			}
			keys, err := pinHostKeys(cmd.Context(), f, yes)
			if err != nil {
				if f.Auth.Mode == forge.ModeDeployKey {
					return err
				}
				fmt.Fprintf(os.Stderr, "warning: %v (only needed for deploy-key mode)\n", err)
			}
			f.HostKeys = keys
			if err := forge.Update(m.P.Forges(), func(r *forge.Registry) error {
				r.Forges = append(r.Forges, f)
				return nil
			}); err != nil {
				return err
			}
			fmt.Printf("added forge %s (%s); check credentials with: curral forge test %s\n", f.Name, f.WebURL(), f.Name)
			return nil
		},
	}
	fl := add.Flags()
	fl.StringVar(&f.Type, "type", "", "github or gitea")
	fl.StringVar(&f.Host, "host", "", "forge host name, e.g. github.com")
	fl.StringVar(&f.HTTPURL, "http-url", "", "web/API base URL (default https://<host>)")
	fl.IntVar(&f.SSHPort, "ssh-port", 0, "SSH port (default 22)")
	fl.StringVar(&f.Auth.Mode, "mode", "", "deploy-key | minted-token | token | none")
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
	if f.Type == forge.GitHub && f.Host == "github.com" {
		keys, err := (&forge.Client{F: f}).GitHubKeys(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching GitHub host keys: %w", err)
		}
		fmt.Printf("pinned %d GitHub host keys from api.github.com/meta\n", len(keys))
		return keys, nil
	}
	keys, err := forge.Scan(ctx, f.Host, f.Port())
	if err != nil {
		return nil, err
	}
	fmt.Printf("SSH host keys of %s port %d:\n", f.Host, f.Port())
	for _, k := range keys {
		fmt.Printf("  %s\n", forge.Fingerprint(k))
	}
	if !yes {
		fmt.Print("Do these match the forge's published fingerprints? Trust them? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, fmt.Errorf("host keys not trusted")
		}
	}
	var out []string
	for _, k := range keys {
		out = append(out, forge.FormatHostKey(k))
	}
	return out, nil
}

func testForge(ctx context.Context, m *vm.Manager, f forge.Forge) error {
	sec := &creds.Resolver{Sources: m.Cfg.Secrets, File: m.P.SecretsFile()}
	get := func(ref string) (string, error) {
		name := strings.TrimPrefix(ref, "secret:")
		v, ok, err := sec.Lookup(ctx, name)
		if err == nil && !ok {
			err = fmt.Errorf("secret %s not found on the host", name)
		}
		return v, err
	}
	c := &forge.Client{F: f}
	switch {
	case f.Auth.Token != "":
		tok, err := get(f.Auth.Token)
		if err != nil {
			return err
		}
		c.Token = tok
	case f.Auth.Password != "":
		pw, err := get(f.Auth.Password)
		if err != nil {
			return err
		}
		c.User, c.Password, c.Basic = f.Auth.User, pw, true
	}
	if c.Token != "" || c.Basic {
		login, err := c.Login(ctx)
		if err != nil {
			return fmt.Errorf("API: %w", err)
		}
		fmt.Printf("API ok: authenticated as %s\n", login)
	}
	if len(f.HostKeys) > 0 && !(f.Type == forge.GitHub && f.Host == "github.com") {
		keys, err := forge.Scan(ctx, f.Host, f.Port())
		if err != nil {
			return fmt.Errorf("SSH: %w", err)
		}
		pinned := map[string]bool{}
		for _, k := range f.HostKeys {
			pinned[k] = true
		}
		for _, k := range keys {
			if !pinned[forge.FormatHostKey(k)] {
				return fmt.Errorf("SSH host key changed: %s is not pinned (if expected: curral forge rm %s && curral forge add ...)", forge.Fingerprint(k), f.Name)
			}
		}
		fmt.Println("SSH host keys match")
	}
	return nil
}

func pullCmd() *cobra.Command {
	var o vm.PullOpts
	c := &cobra.Command{
		Use:   "pull <name>",
		Short: "Fetch a branch from a VM's repo into refs/curral/<vm>/<branch> of a host repo",
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
	f.StringVar(&o.Head, "head", "", "branch name on the forge (default curral/<vm>/<branch>)")
	return c
}
