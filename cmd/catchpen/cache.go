package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/catchpen/internal/cache"
)

func cacheCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cache",
		Short: "Show the package caches",
		Long: `Shows what the host pull-through package caches hold. Templates list the caches
their VMs use (cache: [apt, go, npm, pypi, docker]); config.yaml cache_max caps their
size (default 20G): the caches trim themselves every 10 minutes while fetching, and
catchpen gc trims them too.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			us, err := cache.Store{Dir: m.P.Pkg()}.Usage()
			if err != nil {
				return err
			}
			var total int64
			for _, u := range us {
				total += u.Bytes
				fmt.Printf("%-7s %7d files  %s\n", u.Kind, u.Files, humanBytes(u.Bytes))
			}
			fmt.Printf("total   %s of %s (%s)\n", humanBytes(total), humanBytes(m.Cfg.CacheMaxBytes()), m.P.Pkg())
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "clear [kind...]",
		Short: "Empty the package caches (all, or the kinds given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			return cache.Store{Dir: m.P.Pkg()}.Clear(args...)
		},
	})
	return c
}

func cacheStdioCmd() *cobra.Command {
	var dir, name, store, kinds string
	var max int64
	c := &cobra.Command{
		Use:    "cache-stdio",
		Short:  "Serve one guest package cache connection on stdin/stdout (spawned by QEMU)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := stdioConn()
			if err != nil {
				return err
			}
			s := &cache.Server{Store: cache.Store{Dir: store}, Kinds: map[string]bool{}, Dir: dir, VM: name, Max: max,
				OnCanary: canaryHit(name)}
			for _, k := range strings.Split(kinds, ",") {
				s.Kinds[k] = true
			}
			return s.ServeConn(cmd.Context(), conn)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "VM directory")
	c.Flags().StringVar(&name, "vm", "", "VM name")
	c.Flags().StringVar(&store, "store", "", "cache directory")
	c.Flags().StringVar(&kinds, "kinds", "", "caches the VM uses, comma separated")
	c.Flags().Int64Var(&max, "max", 0, "trim the store to this many bytes (0: never)")
	for _, f := range []string{"dir", "vm", "store", "kinds"} {
		_ = c.MarkFlagRequired(f)
	}
	return c
}
