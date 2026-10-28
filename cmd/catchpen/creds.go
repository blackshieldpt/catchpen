package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func credsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "creds",
		Short: "Agent credentials and config inside VMs",
	}
	var all bool
	push := &cobra.Command{
		Use:   "push <name>... | --all",
		Short: "Push credentials, env and agent config into running VMs (also done at every start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return fmt.Errorf("give VM names or --all")
			}
			m, err := manager()
			if err != nil {
				return err
			}
			if all {
				vms, err := m.List()
				if err != nil {
					return err
				}
				for _, v := range vms {
					if v.Running {
						args = append(args, v.Name)
					}
				}
			}
			var errs []error
			for _, n := range args {
				if err := m.Push(cmd.Context(), n); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", n, err))
				}
			}
			return errors.Join(errs...)
		},
	}
	push.Flags().BoolVar(&all, "all", false, "all running VMs")
	c.AddCommand(push)
	return c
}
