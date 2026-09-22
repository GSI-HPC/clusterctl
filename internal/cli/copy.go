// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newCopyCommand(r *root) *cobra.Command {
	var (
		user      string
		root      bool
		download  bool
		recursive bool
		preserve  bool
	)

	cmd := leaf("copy [-n NODESET] SOURCE... DESTINATION", "Copy files to or from many nodes", `
Copy files to every node of a set, or collect a file from each of them.

By default the sources are local and the destination is on the nodes. With
--download the direction is reversed, and because every node would write to
the same local path, the destination must be a directory; each file lands in
it under the node's name.

  clusterctl copy -n @exe /etc/hosts /etc/hosts
  clusterctl copy -n exe[1-4] --download /var/log/slurmd.log ./logs/`,
		cobra.MinimumNArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			sources, destination := args[:len(args)-1], args[len(args)-1]

			ns, err := a.Select("")
			if err != nil {
				return err
			}
			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			for i := range targets {
				if user != "" {
					targets[i].User = user
				}
				if root {
					targets[i].User = "root"
				}
			}

			if download && ns.Len() > 1 && !strings.HasSuffix(destination, "/") {
				return exitcode.Errorf(exitcode.Usage,
					"downloading from %d nodes needs a directory as the destination, ending in a slash", ns.Len())
			}

			verb := "copy files to"
			if download {
				verb = "collect files from"
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    verb,
				Targets: ns,
				Detail:  fmt.Sprintf("%s -> %s", strings.Join(sources, " "), destination),
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}

			results := make([]*transport.Result, 0, len(targets))
			for _, target := range targets {
				dest := destination
				if download && ns.Len() > 1 {
					dest = destination + target.Name + "-" + baseName(sources[0])
				}
				res, err := a.SSH.Copy(a.Context(), target, transport.CopyRequest{
					Sources:     sources,
					Destination: dest,
					Upload:      !download,
					Recursive:   recursive,
					Preserve:    preserve,
				})
				if err != nil {
					return err
				}
				results = append(results, res)
			}
			if err := a.Print(output.Result{Object: results, Table: resultsTable(results)}); err != nil {
				return err
			}
			return failureError(results)
		})

	flags := cmd.Flags()
	flags.StringVarP(&user, "user", "u", "", "remote account to copy as")
	flags.BoolVarP(&root, "root", "r", false, "copy as root")
	flags.BoolVar(&download, "download", false, "copy from the nodes instead of to them")
	flags.BoolVarP(&recursive, "recursive", "R", false, "copy directories")
	flags.BoolVarP(&preserve, "preserve", "p", false, "keep modification times and modes")
	return cmd
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
