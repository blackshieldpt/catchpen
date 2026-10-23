package main

import (
	"fmt"
	"runtime"
	rtdebug "runtime/debug"

	"github.com/spf13/cobra"
)

// Set by the release build (-ldflags -X); other builds read what they can
// from the build info: go build the commit, go install the module version.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print curral's version, commit and commit date",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			v, c, d, dirty := version, commit, date, false
			if bi, ok := rtdebug.ReadBuildInfo(); ok && c == "" {
				if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
					v = bi.Main.Version
				}
				for _, s := range bi.Settings {
					switch s.Key {
					case "vcs.revision":
						c = s.Value
					case "vcs.time":
						d = s.Value
					case "vcs.modified":
						dirty = s.Value == "true"
					}
				}
			}
			if c == "" {
				c = "unknown"
			}
			if dirty {
				c += "-dirty"
			}
			if d == "" {
				d = "unknown"
			}
			fmt.Printf("curral %s\ncommit %s\ndate   %s\n%s %s/%s\n", v, c, d, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}
