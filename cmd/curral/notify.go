package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackshieldpt/curral/internal/notify"
)

func notifyStdioCmd() *cobra.Command {
	var dir, name string
	var isTask bool
	c := &cobra.Command{
		Use:    "notify-stdio",
		Short:  "Serve one guest notification request on stdin/stdout (spawned by QEMU)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := stdioConn()
			if err != nil {
				return err
			}
			logErr := func(err error) {
				if err == nil {
					return
				}
				if f, ferr := os.OpenFile(filepath.Join(dir, "notify.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
					fmt.Fprintf(f, "%s %v\n", time.Now().UTC().Format(time.RFC3339), err)
					f.Close()
				}
			}
			// The config is only loaded for requests within the rate limit.
			get := func() (*notify.Notifier, error) {
				if isTask {
					return nil, nil // accepted and dropped
				}
				m, err := manager()
				if err != nil {
					return nil, err
				}
				return m.Notifier(), nil
			}
			lim := notify.Limiter{Path: filepath.Join(dir, "notify.bucket"), Burst: 5, Every: 30 * time.Second}
			logErr(notify.ServeGuest(cmd.Context(), conn, name, lim, get, notify.Withhold(dir, canaryHit(name))))
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "VM directory")
	c.Flags().StringVar(&name, "vm", "", "VM name")
	c.Flags().BoolVar(&isTask, "task", false, "the VM runs a headless task")
	return c
}
