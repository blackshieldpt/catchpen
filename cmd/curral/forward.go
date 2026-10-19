package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func forwardCmd() *cobra.Command {
	var local, remote, dynamic []string
	var list, stop bool
	c := &cobra.Command{
		Use:   "forward <name> [-L PORT|H:G|H:HOST:G]... [-R PORT|G:H|G:HOST:H]... [-D PORT]... | --list | --stop",
		Short: "Forward ports over SSH in the background (host listeners on 127.0.0.1)",
		Long: `-L exposes a guest port on the host's 127.0.0.1. -R exposes a host service to the
guest (it bypasses the egress proxy, so it is logged in the VM's egress log). -D runs a
SOCKS proxy on the host into the guest. Sessions end with --stop or when the VM stops.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			name := args[0]
			switch {
			case stop:
				return m.StopForwards(name)
			case list:
				fs, err := m.Forwards(name)
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "PID\tSTARTED\tFORWARDS")
				for _, f := range fs {
					fmt.Fprintf(tw, "%d\t%s\t%s\n", f.PID, f.Started.Local().Format("15:04:05"), strings.Join(f.Specs, ", "))
				}
				return tw.Flush()
			}
			fs, err := m.Forward(cmd.Context(), name, local, remote, dynamic)
			if err != nil {
				return err
			}
			fmt.Printf("forwarding (pid %d): %s\n", fs.PID, strings.Join(fs.Specs, ", "))
			return nil
		},
	}
	f := c.Flags()
	f.StringArrayVarP(&local, "local", "L", nil, "guest port to the host")
	f.StringArrayVarP(&remote, "remote", "R", nil, "host service to the guest")
	f.StringArrayVarP(&dynamic, "dynamic", "D", nil, "SOCKS proxy port on the host")
	f.BoolVar(&list, "list", false, "list forward sessions")
	f.BoolVar(&stop, "stop", false, "stop all forward sessions")
	return c
}
