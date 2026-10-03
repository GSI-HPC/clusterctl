// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/GSI-HPC/go-nodeset"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newHostkeyCommand(r *root) *cobra.Command {
	return group("hostkey", "Maintain the host key file the site trusts", `
Every connection is checked against one host key file, which the team keeps
under version control. These commands collect keys, compare them with the
file and write the file back.

The file is always rewritten completely, under a lock, so two administrators
refreshing at the same time cannot lose an entry.`,
		newHostkeyScanCommand(r),
		newHostkeyVerifyCommand(r),
		newHostkeyRefreshCommand(r),
		newHostkeyRemoveCommand(r),
		newHostkeyListCommand(r),
	)
}

// scanDial replaces the scanner's direct connection; only the tests set it.
var scanDial func(ctx context.Context, network, address string) (net.Conn, error)

// scanTargets collects the current key of every host of a node set.
//
// The hosts are scanned in parallel, bounded like any fan-out, so that nodes
// still in the installer cost one timeout between them rather than one each.
// The scan is reported as the step "scan the host keys", with a target for
// each node; one whose host has no name ends failed, with the reason, and
// first, so that an interrupt does not end it as interrupted.
func scanTargets(ctx context.Context, a *app.App, ns *nodeset.NodeSet, bmc bool, timeout time.Duration) (map[string][]hostkeys.Entry, map[string]error) {
	resolve := a.Namer.FQDN
	if bmc {
		resolve = a.BMCHost
	}
	// scan is the scan of one node's host, or why there is none.
	type scan struct {
		node, host string
		scanner    *hostkeys.Scanner
		err        error
	}
	nodes := ns.Expand()
	scans := make([]scan, len(nodes))
	for i, node := range nodes {
		host, err := resolve(node)
		if err != nil {
			scans[i] = scan{node: node, err: err}
			continue
		}
		hops, err := jumpHops(a, host)
		if err != nil {
			scans[i] = scan{node: node, err: err}
			continue
		}
		scanner := &hostkeys.Scanner{Timeout: timeout, Dial: scanDial}
		if len(hops) > 0 {
			scanner.Dial = dialThrough(a, hops)
		}
		scans[i] = scan{node: node, host: host, scanner: scanner}
	}
	// The nodes whose host has no name go first: they end at once, with
	// their reason, rather than as interrupted when an interrupt comes
	// before their turn. Nothing printed depends on the order.
	slices.SortStableFunc(scans, func(x, y scan) int {
		return cmp.Compare(boolRank(x.err == nil), boolRank(y.err == nil))
	})

	outcomes, _ := fanout.Map(ctx, scans, fanout.MapOptions[scan]{
		Step:     "scan the host keys",
		Limit:    a.Spec.Fanout.Max,
		Describe: func(s scan) fanout.Item { return fanout.Item{Node: s.node, Host: s.host} },
		PanicLog: a.WorkerDiag,
	}, func(ctx context.Context, s scan) ([]hostkeys.Entry, error) {
		if s.err != nil {
			return nil, s.err
		}
		return scanHost(ctx, a.WorkerDiag, s.scanner, s.host)
	})
	found := map[string][]hostkeys.Entry{}
	failed := map[string]error{}
	for i, o := range outcomes {
		switch s := scans[i]; {
		case s.err != nil:
			failed[s.node] = s.err
		case o.Started && o.Err != nil:
			failed[s.host] = o.Err
		case o.Started:
			found[s.host] = o.Value
		}
	}
	// A host the interrupt came before did not answer either, unless
	// another node that shares it was scanned.
	for i, o := range outcomes {
		host := scans[i].host
		_, ok := found[host]
		if _, bad := failed[host]; !o.Started && scans[i].err == nil && !ok && !bad {
			failed[host] = o.Err
		}
	}
	return found, failed
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scanHost scans one host. A panic in the scan becomes the host's failure,
// with its stack written to log under the name of the host, which is what
// the tables name too.
func scanHost(ctx context.Context, log io.Writer, scanner *hostkeys.Scanner, host string) (entries []hostkeys.Entry, err error) {
	defer func() {
		if p := fanout.Recovered(log, host, recover()); p != nil {
			entries, err = nil, p
		}
	}()
	return scanner.Scan(ctx, host)
}

// jumpHops returns the jump hosts a host is reached through: those of the
// role serving it, the first role in name order deciding as it does in the
// generated ssh configuration, resolved as that configuration resolves them.
func jumpHops(a *app.App, host string) ([]transport.Hop, error) {
	for _, name := range a.RoleNames() {
		role := a.Spec.Hosts[name]
		if !strings.EqualFold(role.Host, host) {
			continue
		}
		if role.ProxyJump == "" {
			return nil, nil
		}
		hops, err := a.SSH.Jumps(role.ProxyJump)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Usage, fmt.Errorf("role %s: %w", name, err))
		}
		return hops, nil
	}
	return nil, nil
}

// dialThrough reaches a host the way ssh would, through its jump hosts with
// "ssh -W". The jumps are made with the generated configuration, so their
// own host keys are checked against the site's file before anything passes
// through them. BatchMode keeps a scan of many hosts from prompting.
//
// The last hop is given as ssh gives it when it follows a ProxyJump itself,
// its account with -l and its port with -p: a destination cannot carry a
// port, and without an account ssh would log in as the local user.
func dialThrough(a *app.App, hops []transport.Hop) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, _, address string) (net.Conn, error) {
		config, err := a.SSH.ConfigPath()
		if err != nil {
			return nil, err
		}
		argv := []string{a.SSH.Binary(), "-F", config, "-o", "BatchMode=yes"}
		last := hops[len(hops)-1]
		if before := hops[:len(hops)-1]; len(before) > 0 {
			chain := make([]string, len(before))
			for i, hop := range before {
				chain[i] = hop.String()
			}
			argv = append(argv, "-J", strings.Join(chain, ","))
		}
		if last.User != "" {
			argv = append(argv, "-l", last.User)
		}
		if last.Port != "" {
			argv = append(argv, "-p", last.Port)
		}
		argv = append(argv, "-W", address, "--", last.Host)
		return hostkeys.DialCommand(ctx, argv)
	}
}

func hostkeyFile(a *app.App) (string, error) {
	path := a.Path(a.Spec.SSH.KnownHostsFile)
	if path == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no host key file is configured; set ssh.knownHostsFile in the site document")
	}
	return path, nil
}

func newHostkeyScanCommand(r *root) *cobra.Command {
	var (
		bmc     bool
		timeout time.Duration
	)
	cmd := leaf("scan [NODESET]", "Collect the host keys a set of hosts offers", `
Connect to each host, read the key it presents and print it. Nothing is
written: this is what to run before deciding whether a change is expected.

The handshake is abandoned as soon as the key has been seen, so no
credentials are involved.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			found, failed := scanTargets(a.Context(), a, ns, bmc, timeout)

			t := output.NewTable(output.Cols("HOST", "TYPE", "KEY")...)
			for _, host := range slices.Sorted(maps.Keys(found)) {
				for _, e := range found[host] {
					t.Add(host, e.Type, abbreviate(e.Key))
				}
			}
			for _, host := range slices.Sorted(maps.Keys(failed)) {
				t.Add(host, "unreachable", failed[host].Error())
			}
			if err := a.Print(output.Result{Table: t, Object: found}); err != nil {
				return err
			}
			if len(failed) > 0 {
				return exitcode.Errorf(exitcode.Transport, "%d of %d hosts did not answer", len(failed), ns.Len())
			}
			return nil
		}))
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "scan the service processors instead of the nodes")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "how long to wait for one host")
	return cmd
}

func newHostkeyVerifyCommand(r *root) *cobra.Command {
	var (
		bmc     bool
		timeout time.Duration
	)
	cmd := leaf("verify [NODESET]", "Compare the hosts with the host key file", `
Collect the key each host currently offers and compare it with the file.

A host whose key changed is reported and the command exits non-zero. That is
either a reinstalled machine or something worth investigating, and it is not
for this command to decide which.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			path, err := hostkeyFile(a)
			if err != nil {
				return err
			}
			file, err := hostkeys.Load(path)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			found, failed := scanTargets(a.Context(), a, ns, bmc, timeout)

			t := output.NewTable(output.Cols("HOST", "STATUS", "DETAIL")...)
			changed, missing, revoked := 0, 0, 0
			index := file.Index()
			for _, host := range slices.Sorted(maps.Keys(found)) {
				known := index.Find(host)
				switch {
				case anyRevoked(index, host, found[host]):
					// ssh refuses a revoked key whatever else the file
					// says, so a match beside it is no match.
					revoked++
					t.Add(host, "REVOKED", fmt.Sprintf("host offers %s, which the file revokes",
						abbreviate(found[host][0].Key)))
				case len(known) == 0:
					missing++
					t.Add(host, "not in the file", found[host][0].Type)
				case matches(known, found[host]):
					t.Add(host, "matches", known[0].Type)
				default:
					changed++
					t.Add(host, "CHANGED", fmt.Sprintf("file has %s, host offers %s",
						abbreviate(known[0].Key), abbreviate(found[host][0].Key)))
				}
			}
			for _, host := range slices.Sorted(maps.Keys(failed)) {
				t.Add(host, "unreachable", failed[host].Error())
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			switch {
			case revoked > 0:
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d host%s offer a revoked key; investigate before trusting them", revoked, plural(revoked))
			case changed > 0:
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d host key%s changed; check before refreshing", changed, plural(changed))
			case missing > 0:
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d host%s are not in the file; add them with \"clusterctl hostkey refresh\"", missing, plural(missing))
			case len(failed) > 0:
				return exitcode.Errorf(exitcode.Transport, "%d hosts did not answer", len(failed))
			}
			return nil
		}))
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "verify the service processors instead of the nodes")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "how long to wait for one host")
	return cmd
}

func newHostkeyRefreshCommand(r *root) *cobra.Command {
	var (
		bmc     bool
		timeout time.Duration
	)
	cmd := leaf("refresh [NODESET]", "Write the current host keys into the file", `
Collect the key each host offers and write it into the host key file,
replacing what was there.

This rewrites the site's trust anchor, so it asks first. Run
"clusterctl hostkey verify" beforehand and look at what changed: a key that
changed without a reinstall is worth understanding before it is trusted.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			path, err := hostkeyFile(a)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "replace the host keys of",
				Targets: ns,
				Detail:  "writes " + path,
			}); err != nil {
				return err
			}

			found, failed := scanTargets(a.Context(), a, ns, bmc, timeout)
			written := 0
			refused := map[string]bool{}
			err = hostkeys.Modify(a.Context(), path, func(f *hostkeys.File) error {
				// Replacing host keys leaves the revocations as they are,
				// so one index of the file answers for every host.
				index := f.Index()
				var replace []string
				for _, host := range slices.Sorted(maps.Keys(found)) {
					// A revoked key is never written back as trusted, and
					// the entry already there stays as it is.
					if anyRevoked(index, host, found[host]) {
						refused[host] = true
						continue
					}
					replace = append(replace, host)
					written += len(found[host])
				}
				f.ReplaceAll(replace, func(host string) []hostkeys.Entry { return found[host] })
				return nil
			})
			if err != nil {
				return err
			}

			t := output.NewTable(output.Cols("HOST", "STATUS", "TYPE")...)
			for _, host := range slices.Sorted(maps.Keys(found)) {
				if refused[host] {
					t.Add(host, "REVOKED", found[host][0].Type)
					continue
				}
				t.Add(host, "written", found[host][0].Type)
			}
			for _, host := range slices.Sorted(maps.Keys(failed)) {
				t.Add(host, "unreachable", failed[host].Error())
			}
			t.Caption = fmt.Sprintf("%d keys written to %s", written, path)
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			if len(refused) > 0 {
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d host%s offer a revoked key, which was not written", len(refused), plural(len(refused)))
			}
			if len(failed) > 0 {
				return exitcode.Errorf(exitcode.Transport, "%d hosts did not answer", len(failed))
			}
			return nil
		}))
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "refresh the service processors instead of the nodes")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "how long to wait for one host")
	return cmd
}

func newHostkeyRemoveCommand(r *root) *cobra.Command {
	var bmc bool
	cmd := leaf("remove [NODESET]", "Remove hosts from the host key file", `
Drop every entry for the named hosts. Use this before reinstalling a node, so
that the key it comes back with can be collected cleanly.

An entry written under a node's short name, from before the naming rules were
in place, is dropped too, except with --bmc: that is the node's own key, not
its service processor's.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			path, err := hostkeyFile(a)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "remove the host keys of",
				Targets: ns,
				Detail:  "writes " + path,
			}); err != nil {
				return err
			}

			removed := 0
			t := output.NewTable(output.Cols("HOST", "REMOVED")...)
			err = hostkeys.Modify(a.Context(), path, func(f *hostkeys.File) error {
				// Each node's names, the host name and then the short one,
				// go in one pass over the file.
				var (
					names  []string
					shorts []int
				)
				for _, node := range ns.Expand() {
					var name string
					if bmc {
						name, err = a.BMCHost(node)
					} else {
						name, err = a.Namer.FQDN(node)
					}
					if err != nil {
						return err
					}
					names = append(names, name)
					// A short name may have been written before the naming
					// rules were in place. It is the node's own name, so
					// its key is the node's, whatever --bmc says.
					shorts = append(shorts, -1)
					if !bmc {
						names = append(names, node)
						shorts[len(shorts)-1] = len(names) - 1
					}
				}
				counts := f.RemoveAll(names)
				i := 0
				for _, short := range shorts {
					name, n := names[i], counts[i]
					i++
					if short >= 0 {
						n += counts[short]
						i++
					}
					removed += n
					t.Add(name, fmt.Sprint(n))
				}
				return nil
			})
			if err != nil {
				return err
			}
			t.Caption = fmt.Sprintf("%d entries removed from %s", removed, path)
			return a.Print(output.Result{Table: t})
		}))
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "remove the service processors instead of the nodes")
	return cmd
}

func newHostkeyListCommand(r *root) *cobra.Command {
	return leaf("list", "List the host key file", `
Print the entries of the host key file. MARKER holds the marker of a line
that revokes a key or names a certificate authority: a revoked key is not a
key of its host but one ssh refuses from it.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			path, err := hostkeyFile(a)
			if err != nil {
				return err
			}
			file, err := hostkeys.Load(path)
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("HOST", "MARKER", "TYPE", "KEY")...)
			for _, e := range file.Entries {
				t.Add(strings.Join(e.Hosts, ","), e.Marker, e.Type, abbreviate(e.Key))
			}
			t.Caption = fmt.Sprintf("%d entries in %s", len(file.Entries), path)
			return a.Print(output.Result{Table: t, Object: file.Entries})
		}))
}

// revocations is a host key file, or an index of one, that can say whether
// it revokes a key.
type revocations interface {
	Revoked(host string, key hostkeys.Entry) bool
}

// anyRevoked reports whether the file revokes a key a host offers.
func anyRevoked(f revocations, host string, found []hostkeys.Entry) bool {
	for _, e := range found {
		if f.Revoked(host, e) {
			return true
		}
	}
	return false
}

// matches reports whether every collected key is already in the file.
func matches(known, found []hostkeys.Entry) bool {
	for _, f := range found {
		hit := false
		for _, k := range known {
			if k.Type == f.Type && k.Key == f.Key {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// abbreviate shortens a key for a table, keeping enough to compare by eye.
func abbreviate(key string) string {
	if len(key) <= 24 {
		return key
	}
	return key[:12] + "..." + key[len(key)-8:]
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
