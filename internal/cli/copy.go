// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// defaultCopyTimeout bounds one transfer when neither --timeout nor
// fanout.commandTimeout says otherwise. A transfer stalled on a live
// connection would otherwise hold its slot until Ctrl-C.
const defaultCopyTimeout = 30 * time.Minute

func newCopyCommand(r *root) *cobra.Command {
	var (
		user      string
		root      bool
		download  bool
		recursive bool
		preserve  bool
		timeout   time.Duration
	)

	cmd := leaf("copy [-n NODESET] SOURCE... DESTINATION", "Copy files to or from many nodes", `
Copy files to every node of a set, or collect files from each of them.

By default the sources are local and the destination is on the nodes. With
--download the direction is reversed, and because every node would write to
the same local path, the destination must be a directory; each node's files
land in a directory of its own under it, named after the node.

The nodes are copied in parallel, as many at once as the fan-out allows, and
each transfer is bounded by --timeout.

A remote path is read by a shell under the legacy scp protocol, which OpenSSH
used by default before 9.0, and taken literally under SFTP. A remote path
holding whitespace, quotes or shell syntax would mean different things to the
two, so it is refused. Globs and ~ work in both.

  clusterctl copy -n @exe /etc/hosts /etc/hosts
  clusterctl copy -n exe[1-4] --download /var/log/slurmd.log /var/log/messages ./logs/`,
		cobra.MinimumNArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			sources, destination := args[:len(args)-1], args[len(args)-1]
			remotes := []string{destination}
			if download {
				remotes = sources
			}
			for _, path := range remotes {
				if err := checkRemotePath(path); err != nil {
					return err
				}
			}

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

			perNode := download && ns.Len() > 1
			if perNode && !strings.HasSuffix(destination, "/") {
				return exitcode.Errorf(exitcode.Usage,
					"downloading from %d nodes needs a directory as the destination, ending in a slash", ns.Len())
			}
			// destinationFor is where a node's files go: a directory of its
			// own when several nodes write to the same place.
			destinationFor := func(t transport.Target) string {
				if perNode {
					return destination + t.Name + "/"
				}
				return destination
			}

			limit := timeout
			if limit <= 0 {
				limit = a.Timeout().Get()
			}
			if limit <= 0 {
				limit = defaultCopyTimeout
			}

			verb := "copy files to"
			detail := fmt.Sprintf("%s -> %s", strings.Join(sources, " "), destination)
			if download {
				verb = "collect files from"
				if perNode {
					detail = fmt.Sprintf("%s -> %sNODE/", strings.Join(sources, " "), destination)
				}
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    verb,
				Targets: ns,
				Detail:  detail,
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}

			// The configuration is written once, before the fan-out, so
			// that a failure to write it is one error rather than one per
			// node.
			if _, err := a.SSH.ConfigPath(); err != nil {
				return err
			}
			if perNode {
				for _, t := range targets {
					if err := os.MkdirAll(destinationFor(t), 0o755); err != nil {
						return exitcode.Wrap(exitcode.Usage, err)
					}
				}
			}

			executor := a.Executor()
			executor.Runner = copyRunner{
				client:  a.SSH,
				timeout: limit,
				request: func(t transport.Target) transport.CopyRequest {
					return transport.CopyRequest{
						Sources:     sources,
						Destination: destinationFor(t),
						Upload:      !download,
						Recursive:   recursive,
						Preserve:    preserve,
					}
				},
			}
			results := executor.Run(a.Context(), targets, transport.Request{})
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
	flags.DurationVar(&timeout, "timeout", 0,
		"how long one transfer may take (default: fanout.commandTimeout, or 30m)")
	return cmd
}

// copyRunner lets the fan-out executor run transfers, so that copy honours
// fanout.max and --fanout like every other command that reaches many nodes.
type copyRunner struct {
	client  *transport.Client
	timeout time.Duration
	request func(transport.Target) transport.CopyRequest
}

// Run copies to or from one target; the request is ignored in favour of the
// target's transfer.
func (c copyRunner) Run(ctx context.Context, target transport.Target, _ transport.Request) (*transport.Result, error) {
	transferCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	res, err := c.client.Copy(transferCtx, target, c.request(target))
	if err != nil {
		return nil, err
	}
	if ctx.Err() == nil && errors.Is(transferCtx.Err(), context.DeadlineExceeded) {
		res.ExitCode = -1
		res.Err = exitcode.Wrap(exitcode.Transport,
			fmt.Errorf("%s: the transfer did not finish within %s: %w", target, c.timeout, transferCtx.Err()))
	}
	return res, nil
}

// checkRemotePath refuses a remote path the legacy scp protocol and SFTP
// would read differently: the first hands it to the remote shell, the second
// takes it literally. Globs and a leading ~ mean the same to both.
func checkRemotePath(path string) error {
	for _, r := range path {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("'\"`$\\;&|<>(){}#", r) {
			return exitcode.Errorf(exitcode.Usage,
				"remote path %q holds %q, which the legacy scp protocol would hand to the remote shell "+
					"and SFTP would not; only globs and ~ are read the same by both", path, r)
		}
	}
	return nil
}
