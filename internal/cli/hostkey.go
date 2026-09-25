// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/nodeset"
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
func scanTargets(a *app.App, ns *nodeset.NodeSet, bmc bool, timeout time.Duration) (map[string][]hostkeys.Entry, map[string]error) {
	found := map[string][]hostkeys.Entry{}
	failed := map[string]error{}
	resolve := a.Namer.FQDN
	if bmc {
		resolve = a.BMCHost
	}
	var hosts []string
	var scanners []*hostkeys.Scanner
	for _, node := range ns.Expand() {
		host, err := resolve(node)
		if err != nil {
			failed[node] = err
			continue
		}
		scanner := &hostkeys.Scanner{Timeout: timeout, Dial: scanDial}
		if hops := jumpHops(a, host); len(hops) > 0 {
			scanner.Dial = dialThrough(a, hops)
		}
		hosts, scanners = append(hosts, host), append(scanners, scanner)
	}

	var mu sync.Mutex
	fanout.Each(a.Context(), len(hosts), a.Spec.Fanout.Max, func(i int) {
		entries, err := scanHost(a.Context(), scanners[i], hosts[i])
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed[hosts[i]] = err
			return
		}
		found[hosts[i]] = entries
	})
	// A host the interrupt came before did not answer either.
	for _, host := range hosts {
		_, ok := found[host]
		if _, bad := failed[host]; !ok && !bad {
			failed[host] = a.Context().Err()
		}
	}
	return found, failed
}

// scanHost scans one host. A panic in the scan becomes the host's failure
// rather than the end of the process.
func scanHost(ctx context.Context, scanner *hostkeys.Scanner, host string) (entries []hostkeys.Entry, err error) {
	defer func() {
		if p := fanout.Recovered(host, recover()); p != nil {
			entries, err = nil, p
		}
	}()
	return scanner.Scan(ctx, host)
}

// jumpHops returns the jump hosts a host is reached through: those of the
// role serving it, the first role in name order deciding as it does in the
// generated ssh configuration. A role names another role or a host, and a
// chain is separated by commas.
func jumpHops(a *app.App, host string) []string {
	for _, name := range a.RoleNames() {
		role := a.Spec.Hosts[name]
		if !strings.EqualFold(role.Host, host) {
			continue
		}
		if role.ProxyJump == "" {
			return nil
		}
		var hops []string
		for hop := range strings.SplitSeq(role.ProxyJump, ",") {
			hop = strings.TrimSpace(hop)
			if jump, ok := a.Spec.Hosts[hop]; ok && jump.Host != "" {
				hop = jump.Host
			}
			hops = append(hops, hop)
		}
		return hops
	}
	return nil
}

// dialThrough reaches a host the way ssh would, through its jump hosts with
// "ssh -W". The jumps are made with the generated configuration, so their
// own host keys are checked against the site's file before anything passes
// through them. BatchMode keeps a scan of many hosts from prompting.
func dialThrough(a *app.App, hops []string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, _, address string) (net.Conn, error) {
		config, err := a.SSH.ConfigPath()
		if err != nil {
			return nil, err
		}
		argv := []string{a.SSH.Binary(), "-F", config, "-o", "BatchMode=yes"}
		last := len(hops) - 1
		if last > 0 {
			argv = append(argv, "-J", strings.Join(hops[:last], ","))
		}
		argv = append(argv, "-W", address, "--", hops[last])
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
			found, failed := scanTargets(a, ns, bmc, timeout)

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
			found, failed := scanTargets(a, ns, bmc, timeout)

			t := output.NewTable(output.Cols("HOST", "STATUS", "DETAIL")...)
			changed, missing, revoked := 0, 0, 0
			for _, host := range slices.Sorted(maps.Keys(found)) {
				known := file.Find(host)
				switch {
				case anyRevoked(file, host, found[host]):
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

			found, failed := scanTargets(a, ns, bmc, timeout)
			written := 0
			refused := map[string]bool{}
			err = hostkeys.Modify(a.Context(), path, func(f *hostkeys.File) error {
				for _, host := range slices.Sorted(maps.Keys(found)) {
					// A revoked key is never written back as trusted, and
					// the entry already there stays as it is.
					if anyRevoked(f, host, found[host]) {
						refused[host] = true
						continue
					}
					f.Replace(host, found[host])
					written += len(found[host])
				}
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
that the key it comes back with can be collected cleanly.`,
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
					n := f.Remove(name)
					// A short name may have been written before the naming
					// rules were in place.
					n += f.Remove(node)
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
Print the entries of the host key file.`,
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
			t := output.NewTable(output.Cols("HOST", "TYPE", "KEY")...)
			for _, e := range file.Entries {
				t.Add(strings.Join(e.Hosts, ","), e.Type, abbreviate(e.Key))
			}
			t.Caption = fmt.Sprintf("%d entries in %s", len(file.Entries), path)
			return a.Print(output.Result{Table: t, Object: file.Entries})
		}))
}

// anyRevoked reports whether the file revokes a key a host offers.
func anyRevoked(f *hostkeys.File, host string, found []hostkeys.Entry) bool {
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
