package main

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/paths"
	"github.com/blackshieldpt/catchpen/internal/server"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

func daemonInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show what API clients need: the socket, and the TLS certificate's pin (made on first use)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.Default()
			if err := p.Ensure(); err != nil {
				return err
			}
			c, err := server.Certificate(p.ServerKey(), p.ServerCert())
			if err != nil {
				return err
			}
			cs, err := server.LoadClients(p.Clients())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "config:      %s\nsocket:      %s\ncertificate: %s\nclients:     %d\n",
				p.Config, p.APISocket(), api.PublicKeyPin(c.Leaf), len(cs.Clients))
			return nil
		},
	}
}

func daemonClientCmd() *cobra.Command {
	c := &cobra.Command{Use: "client", Short: "Enrol, list and remove the API's TCP clients"}
	var certFile string
	var hmac bool
	var scopes []string
	add := &cobra.Command{
		Use:   "add <name> (--cert <file> | --hmac)",
		Short: "Enrol a client: by its certificate (mTLS), or with a new HMAC key printed once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.Default()
			if err := p.Ensure(); err != nil {
				return err
			}
			name, out, errOut := args[0], cmd.OutOrStdout(), cmd.ErrOrStderr()
			record := func(event string, detail map[string]any) {
				if err := (audit.Log{Path: p.Audit()}).Record(event, "", detail); err != nil {
					fmt.Fprintf(errOut, "warning: audit log: %v\n", err)
				}
			}
			if hmac {
				key, err := server.AddHMAC(p.Clients(), name, scopes)
				if err != nil {
					return err
				}
				record(audit.APIClientAdd, map[string]any{"client": name, "method": server.MethodHMAC, "scopes": scopes})
				// A key that can't be handed over is no use: take it back.
				if _, err := fmt.Fprintln(out, hex.EncodeToString(key)); err != nil {
					if rerr := server.RemoveClient(p.Clients(), name); rerr != nil {
						return fmt.Errorf("writing the key: %w; removing client %s: %w", err, name, rerr)
					}
					record(audit.APIClientRemove, map[string]any{"client": name})
					return fmt.Errorf("writing the key: %w (client %s removed)", err, name)
				}
				fmt.Fprintf(errOut, "enrolled %s (hmac): give it the key above now; catchpen won't show it again\n", name)
				return nil
			}
			cert, err := readCert(certFile)
			if err != nil {
				return err
			}
			pin, err := server.AddMTLS(p.Clients(), name, cert, scopes)
			if err != nil {
				return err
			}
			record(audit.APIClientAdd, map[string]any{"client": name, "method": server.MethodMTLS, "scopes": scopes, "pin": pin})
			fmt.Fprintf(out, "enrolled %s (mtls), certificate key %s\n", name, pin)
			return nil
		},
	}
	add.Flags().StringVar(&certFile, "cert", "", "the client's certificate (PEM)")
	add.Flags().BoolVar(&hmac, "hmac", false, "generate an HMAC key for the client")
	add.Flags().StringSliceVar(&scopes, "scope", nil, "extra permission: "+server.ScopeConfigWrite)
	add.MarkFlagsMutuallyExclusive("cert", "hmac")
	add.MarkFlagsOneRequired("cert", "hmac")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List enrolled clients",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := server.LoadClients(paths.Default().Clients())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMETHOD\tSCOPES\tADDED")
			for _, c := range cs.Clients {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Method, strings.Join(c.Scopes, ","), c.Added.Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a client",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.Default()
			if err := p.Ensure(); err != nil {
				return err
			}
			if err := server.RemoveClient(p.Clients(), args[0]); err != nil {
				return err
			}
			if err := (audit.Log{Path: p.Audit()}).Record(audit.APIClientRemove, "", map[string]any{"client": args[0]}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: audit log: %v\n", err)
			}
			return nil
		},
	}
	c.AddCommand(add, ls, rm)
	return c
}

// readCert reads the first certificate in a PEM file; other blocks (a key,
// say) are decoded and skipped.
func readCert(file string) (*x509.Certificate, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			return nil, fmt.Errorf("%s: no PEM certificate", file)
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			return c, nil
		}
	}
}
