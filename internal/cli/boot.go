// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func newBootCommand(r *root) *cobra.Command {
	return group("boot", "Configure what the nodes boot over the network", `
The PXE service hands a node a boot configuration when it asks over the
network. These commands read and set which configuration each node gets, and
show the logs of the services involved.

A boot path is configured for one request by default. A persistent path is
what leaves a machine reinstalling every time it reboots, so it has to be
asked for.`,
		newBootStatusCommand(r),
		newBootSetCommand(r),
		newBootUnsetCommand(r),
		newBootListCommand(r),
		newBootSyncCommand(r),
		newBootLogCommand(r),
		newBootShellCommand(r),
		newBootGrubCommand(r),
	)
}

// nodeAddress resolves the address a node boots with: what the inventory
// says, else what DHCP hands it.
//
// The address names a file on the PXE and TFTP hosts, where the link to it
// is written as root, so anything that does not parse as an IP address is
// refused rather than joined into a path.
func nodeAddress(a *app.App, node string) (string, error) {
	address, source := "", "the inventory"
	if entry, ok := a.Inventory.Lookup(node); ok && entry.Address != "" {
		address = entry.Address
	} else {
		cfg, err := dhcpConfig(a)
		if err != nil {
			return "", err
		}
		address, err = cfg.BootAddress(node)
		switch {
		case errors.Is(err, dhcp.ErrNoAddress):
		case err != nil:
			return "", exitcode.Wrap(exitcode.Usage, err)
		default:
			source = "DHCP"
		}
	}
	if address == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no address is known for %s; set it in the inventory or in DHCP", node)
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return "", exitcode.Errorf(exitcode.Usage,
			"%s has the address %q in %s, which is not an IP address; the boot link is named after it",
			node, output.EscapeCell(address), source)
	}
	return ip.String(), nil
}

// nodeAddresses resolves the boot address of each node, in order, and
// refuses two nodes that share one: the link named after the address would
// arm both, and one of them was not asked for.
func nodeAddresses(a *app.App, nodes []string) ([]string, error) {
	owners := map[string][]string{}
	for _, n := range a.Inventory.All() {
		if ip := net.ParseIP(n.Address); ip != nil {
			owners[ip.String()] = append(owners[ip.String()], n.Name)
		}
	}
	addresses := make([]string, len(nodes))
	seen := map[string]string{}
	for i, node := range nodes {
		address, err := nodeAddress(a, node)
		if err != nil {
			return nil, err
		}
		if other, ok := seen[address]; ok {
			return nil, sharedAddress(address, other, node)
		}
		for _, other := range owners[address] {
			if other != node {
				return nil, sharedAddress(address, other, node)
			}
		}
		seen[address] = node
		addresses[i] = address
	}
	return addresses, nil
}

func sharedAddress(address, first, second string) error {
	return exitcode.Errorf(exitcode.Usage,
		"%s and %s both have the address %s, so a boot link for one would arm the other; fix the inventory or DHCP",
		first, second, address)
}

// pxeRole returns the host role the PXE service runs on.
func pxeRole(a *app.App) (string, error) {
	role := a.Spec.Services.PXESrv.Role
	if role == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no host role runs the PXE service; set services.pxesrv.role")
	}
	return role, nil
}

// bootLink is the boot path link of one node on the PXE service, and what
// became of it.
type bootLink struct {
	Node    string `json:"node"`
	Address string `json:"address"`
	Path    string `json:"bootPath,omitempty"`
	// Mode is once or persistent; a persistent link carries the static
	// suffix and survives the first request.
	Mode string `json:"mode,omitempty"`
	// Result is set or removed, failed, or unknown when the script stopped
	// before it said.
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

const (
	bootOnce       = "once"
	bootPersistent = "persistent"
)

// resolveBootLinks resolves the address and boot path of every node before
// anything is written, so a node with no boot path stops the command
// instead of leaving half the set configured. An explicit path wins over
// the cluster rules; a rule marked static asks for a persistent link.
func resolveBootLinks(a *app.App, ns *nodeset.NodeSet, explicit string, persistent bool) ([]bootLink, error) {
	nodes := ns.Expand()
	addresses, err := nodeAddresses(a, nodes)
	if err != nil {
		return nil, err
	}
	links := make([]bootLink, len(nodes))
	var persistentNodes []string
	for i, node := range nodes {
		path, static := explicit, persistent
		if path == "" {
			resolved, ruleStatic, err := inventory.BootPath(a.Inventory, a.Spec.BootPaths, node)
			if err != nil {
				return nil, exitcode.Wrap(exitcode.Usage, err)
			}
			path, static = resolved, persistent || ruleStatic
		}
		mode := bootOnce
		if static {
			mode = bootPersistent
			persistentNodes = append(persistentNodes, node)
		}
		links[i] = bootLink{Node: node, Address: addresses[i], Path: path, Mode: mode}
	}
	// Without a suffix the persistent link is the one-shot link, and the
	// node would find no boot path on its second request.
	if len(persistentNodes) > 0 && a.Spec.Services.PXESrv.StaticSuffix == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"%s would get a persistent boot path, but services.pxesrv.staticSuffix is not set, "+
				"so it would be written as the one-shot link", fold(persistentNodes))
	}
	return links, nil
}

// linkName is the file the PXE service looks up for one node.
func linkName(a *app.App, root string, l bootLink) string {
	name := root + "/" + l.Address
	if l.Mode == bootPersistent {
		name += a.Spec.Services.PXESrv.StaticSuffix
	}
	return name
}

// describeBootLinks lists each distinct boot path with the nodes it is
// written for, for the preview: the set may span several installations,
// and each one is confirmed, not only the first.
func describeBootLinks(links []bootLink) string {
	type group struct {
		path, mode string
		nodes      []string
	}
	var groups []*group
	index := map[[2]string]*group{}
	for _, l := range links {
		key := [2]string{l.Path, l.Mode}
		g, ok := index[key]
		if !ok {
			g = &group{path: l.Path, mode: l.Mode}
			index[key] = g
			groups = append(groups, g)
		}
		g.nodes = append(g.nodes, fmt.Sprintf("%s (%s)", l.Node, l.Address))
	}
	lines := make([]string, 0, len(groups))
	for _, g := range groups {
		mode := "for the next request"
		if g.mode == bootPersistent {
			mode = "persistently"
		}
		lines = append(lines, fmt.Sprintf("%s, %s: %s", g.path, mode, strings.Join(g.nodes, ", ")))
	}
	return strings.Join(lines, "\n  ")
}

// checkBootLinks reads the PXE host before anything is written: every boot
// path has to exist, or the machine is reset into a failed network boot,
// and a one-shot link is refused over a persistent one, which the PXE
// service would keep offering after the first request. It only reads, so a
// dry run makes the same check and is refused where the real run would be.
func checkBootLinks(a *app.App, role, root string, links []bootLink) error {
	var paths []string
	seen := map[string]bool{}
	for _, l := range links {
		if !seen[l.Path] {
			seen[l.Path] = true
			paths = append(paths, l.Path)
		}
	}
	var script strings.Builder
	script.WriteString("for p in")
	for _, p := range paths {
		script.WriteString(" " + shellquote.Quote(p))
	}
	script.WriteString("; do [ -f \"$p\" ] || printf '%s\\n' \"$p\"; done\n")
	result, err := a.ReadOnRole(a.Context(), role, transport.Request{
		Script: script.String(),
	})
	if err != nil {
		return err
	}
	var missing []string
	for _, line := range result.Lines() {
		// Only a path that was asked about counts, whatever else the
		// host prints.
		if seen[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) > 0 {
		return exitcode.Errorf(exitcode.Usage, "no boot configuration exists on %s at %s; see \"clusterctl boot list\"",
			role, strings.Join(missing, ", "))
	}

	suffix := a.Spec.Services.PXESrv.StaticSuffix
	if suffix == "" {
		return nil
	}
	existing, err := readBootLinks(a, role, root)
	if err != nil {
		return err
	}
	var held []string
	for _, l := range links {
		if l.Mode == bootOnce && existing[l.Address+suffix] != "" {
			held = append(held, l.Node)
		}
	}
	if len(held) > 0 {
		return exitcode.Errorf(exitcode.Usage,
			"%s has a persistent boot path, which a one-shot one does not replace; "+
				"remove it with \"clusterctl boot unset\" first, or pass --persistent", fold(held))
	}
	return nil
}

// readBootLinks lists the links under the PXE root, name to target, in one
// call rather than one connection per node.
func readBootLinks(a *app.App, role, root string) (map[string]string, error) {
	result, err := a.ReadOnRole(a.Context(), role, transport.Request{
		Argv: []string{"find", root, "-maxdepth", "1", "-type", "l", "-printf", "%f\t%l\n"},
	})
	if err != nil {
		return nil, err
	}
	links := map[string]string{}
	for _, line := range result.Lines() {
		name, target, ok := strings.Cut(line, "\t")
		if ok {
			links[name] = target
		}
	}
	return links, nil
}

// The link scripts try every node and report each one on a line of its
// own, rather than stopping at the first failure under set -e: a failing
// line then neither leaves the rest of the set as it was nor goes
// unreported.
const (
	bootLinkScript = `set -u
bootlink() {
	if [ -d "$3" ] && [ ! -L "$3" ]; then
		printf 'fail\t%s\t%s\n' "$1" "$3 is a directory"
	elif out=$(ln -sfn -- "$2" "$3" 2>&1); then
		printf 'ok\t%s\n' "$1"
	else
		printf 'fail\t%s\t%s\n' "$1" "$(printf %s "$out" | tr '\t\n' '  ')"
	fi
}
`
	bootUnlinkScript = `set -u
bootunlink() {
	i=$1
	shift
	if out=$(rm -f -- "$@" 2>&1); then
		printf 'ok\t%s\n' "$i"
	else
		printf 'fail\t%s\t%s\n' "$i" "$(printf %s "$out" | tr '\t\n' '  ')"
	fi
}
`
)

// writeBootLinks points the PXE service at each node's boot path and
// records the outcome in each link.
func writeBootLinks(a *app.App, role, root string, links []bootLink) error {
	var script strings.Builder
	script.WriteString(bootLinkScript)
	for i, l := range links {
		fmt.Fprintf(&script, "bootlink %d %s %s\n", i, shellquote.Quote(l.Path), shellquote.Quote(linkName(a, root, l)))
	}
	return runLinkScript(a, role, script.String(), links, "set")
}

// removeBootLinks removes the one-shot and the persistent link of each node
// and records the outcome in each link.
func removeBootLinks(a *app.App, role, root string, links []bootLink) error {
	suffix := a.Spec.Services.PXESrv.StaticSuffix
	var script strings.Builder
	script.WriteString(bootUnlinkScript)
	for i, l := range links {
		names := shellquote.Quote(root + "/" + l.Address)
		if suffix != "" {
			names += " " + shellquote.Quote(root+"/"+l.Address+suffix)
		}
		fmt.Fprintf(&script, "bootunlink %d %s\n", i, names)
	}
	return runLinkScript(a, role, script.String(), links, "removed")
}

func runLinkScript(a *app.App, role, script string, links []bootLink, done string) error {
	for i := range links {
		links[i].Result, links[i].Error = "unknown", "the PXE host did not report this link"
	}
	result, runErr := a.RunOnRole(a.Context(), role, transport.Request{
		Script: script,
	})
	if result != nil {
		for _, line := range result.Lines() {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) < 2 {
				continue
			}
			i, err := strconv.Atoi(fields[1])
			if err != nil || i < 0 || i >= len(links) || links[i].Result != "unknown" {
				continue
			}
			switch {
			case fields[0] == "ok" && len(fields) == 2:
				links[i].Result, links[i].Error = done, ""
			case fields[0] == "fail" && len(fields) == 3:
				links[i].Result, links[i].Error = "failed", output.EscapeCell(fields[2])
			}
		}
	}

	var failed, unknown []string
	for _, l := range links {
		switch l.Result {
		case "failed":
			failed = append(failed, l.Node)
		case "unknown":
			unknown = append(unknown, l.Node)
		}
	}
	if len(failed) == 0 && len(unknown) == 0 {
		return nil
	}
	var parts []string
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("the boot link of %s could not be changed", fold(failed)))
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("the boot link of %s was not reported and may or may not have changed", fold(unknown)))
	}
	msg := strings.Join(parts, "; ")
	if runErr != nil {
		return fmt.Errorf("%s: %w", msg, runErr)
	}
	return exitcode.Errorf(exitcode.TargetFailed, "%s", msg)
}

// fold renders node names as a folded set, the way every other message
// names hosts.
func fold(nodes []string) string {
	ns := nodeset.New()
	for _, n := range nodes {
		_ = ns.Add(n)
	}
	return ns.String()
}

func newBootStatusCommand(r *root) *cobra.Command {
	return leaf("status [NODESET]", "Show which boot configuration each node is set to", `
List the boot path configured on the PXE service for each node: the one-shot
link and, when services.pxesrv.staticSuffix is set, the persistent one.

A node whose address cannot be resolved is listed with the reason and fails
the command.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root

			ns, err := a.SelectOptional(strings.Join(args, ","))
			if err != nil {
				return err
			}
			// One listing answers for every node, rather than one connection
			// per node as the shell version did.
			links, err := readBootLinks(a, role, root)
			if err != nil {
				return err
			}

			if ns == nil {
				t := output.NewTable(output.Cols("NODE", "ADDRESS", "BOOT PATH")...)
				object := map[string]string{}
				for name, target := range links {
					t.Add("", output.EscapeCell(name), output.EscapeCell(target))
					object[name] = target
				}
				t.Caption = fmt.Sprintf("%d boot paths configured on %s", len(links), role)
				return a.Print(output.Result{Table: t, Object: object})
			}

			suffix := a.Spec.Services.PXESrv.StaticSuffix
			cols := []string{"NODE", "ADDRESS", "BOOT PATH"}
			if suffix != "" {
				cols = append(cols, "PERSISTENT")
			}
			t := output.NewTable(output.Cols(cols...)...)
			type state struct {
				Address    string `json:"address,omitempty"`
				BootPath   string `json:"bootPath,omitempty"`
				Persistent string `json:"persistentBootPath,omitempty"`
				Error      string `json:"error,omitempty"`
			}
			object := map[string]state{}
			var failed []string
			var firstErr error
			for _, node := range ns.Expand() {
				address, err := nodeAddress(a, node)
				if err != nil {
					object[node] = state{Error: err.Error()}
					row := []string{node, "unknown", ""}
					if suffix != "" {
						row = append(row, "")
					}
					t.Add(row...)
					failed = append(failed, node)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				s := state{Address: address, BootPath: cmp.Or(links[address], "none")}
				row := []string{node, address, output.EscapeCell(s.BootPath)}
				if suffix != "" {
					s.Persistent = cmp.Or(links[address+suffix], "none")
					row = append(row, output.EscapeCell(s.Persistent))
				}
				object[node] = s
				t.Add(row...)
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if firstErr != nil {
				return fmt.Errorf("the boot configuration of %s is not known: %w", fold(failed), firstErr)
			}
			return nil
		}))
}

func newBootSetCommand(r *root) *cobra.Command {
	var persistent bool

	cmd := leaf("set [NODESET] [PATH]", "Set the boot configuration of a node set", `
Point the PXE service at a boot configuration for each node.

Without a path, the boot path rules of the cluster decide, which is what makes
a reinstall a one-liner. A node matched by two rules is an error rather than a
silent first match. A rule marked static writes a persistent link, as
--persistent does; both need services.pxesrv.staticSuffix.

Before it asks, the command resolves every address, checks that each boot
path exists on the PXE host, and refuses a one-shot path for a node that has
a persistent one. The question lists each boot path with its nodes.

Every node is tried, and each one's result is listed; a node that failed or
was not reported fails the command.

  clusterctl boot set -n exe[1-4]
  clusterctl boot set -n exe0001 /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root

			var explicit string
			rest := args
			if len(args) > 0 && strings.HasPrefix(args[len(args)-1], "/") {
				explicit = args[len(args)-1]
				rest = args[:len(args)-1]
			}
			ns, err := selection(a, rest)
			if err != nil {
				return err
			}

			links, err := resolveBootLinks(a, ns, explicit, persistent)
			if err != nil {
				return err
			}
			if err := checkBootLinks(a, role, root, links); err != nil {
				return err
			}
			detail := describeBootLinks(links)

			if err := a.Gate.Confirm(safety.Action{
				Verb:    "set the network boot configuration of",
				Targets: ns,
				Detail:  detail,
			}); err != nil {
				return err
			}

			err = writeBootLinks(a, role, root, links)
			t := output.NewTable(output.Cols("NODE", "ADDRESS", "BOOT PATH", "MODE", "RESULT")...)
			for _, l := range links {
				t.Add(l.Node, l.Address, l.Path, l.Mode, resultText(l))
			}
			if printErr := a.Print(output.Result{Table: t, Object: links}); printErr != nil && err == nil {
				err = printErr
			}
			return err
		}))
	cmd.Flags().BoolVar(&persistent, "persistent", false,
		"keep the boot path after the first request (needs services.pxesrv.staticSuffix)")
	return cmd
}

func resultText(l bootLink) string {
	if l.Error != "" {
		return l.Result + ": " + l.Error
	}
	return l.Result
}

func newBootUnsetCommand(r *root) *cobra.Command {
	return leaf("unset [NODESET]", "Remove the boot configuration of a node set", `
Remove the boot path of each node, so the PXE service stops offering one: the
one-shot link and, when services.pxesrv.staticSuffix is set, the persistent
one.

Every node is tried, and each one's result is listed; a node that failed or
was not reported fails the command.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			// The addresses are resolved before the question, so that
			// what is confirmed is what runs.
			nodes := ns.Expand()
			addresses, err := nodeAddresses(a, nodes)
			if err != nil {
				return err
			}
			links := make([]bootLink, len(nodes))
			for i, node := range nodes {
				links[i] = bootLink{Node: node, Address: addresses[i]}
			}

			if err := a.Gate.Confirm(safety.Action{
				Verb: "remove the network boot configuration of", Targets: ns,
			}); err != nil {
				return err
			}

			err = removeBootLinks(a, role, root, links)
			t := output.NewTable(output.Cols("NODE", "ADDRESS", "RESULT")...)
			for _, l := range links {
				t.Add(l.Node, l.Address, resultText(l))
			}
			if printErr := a.Print(output.Result{Table: t, Object: links}); printErr != nil && err == nil {
				err = printErr
			}
			return err
		}))
}

func newBootListCommand(r *root) *cobra.Command {
	return leaf("list", "List the boot configurations the PXE service offers", `
List the boot configuration files available on the PXE service, which is what
a boot path may point at.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.BootPath
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: []string{"find", path, "-type", "f", "-name", "ipxe.*", "-o", "-type", "f", "-name", "grub.cfg*"},
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("BOOT CONFIGURATION")...)
			for _, line := range result.Lines() {
				t.Add(line)
			}
			t.Caption = fmt.Sprintf("%d configurations under %s", t.Len(), path)
			return a.Print(output.Result{Table: t, Object: result.Lines()})
		}))
}

func newBootSyncCommand(r *root) *cobra.Command {
	return leaf("sync", "Update the boot configurations from version control", `
Pull the boot configuration repository on the PXE service, so the
configurations it offers match what is in version control. The pull is
previewed and confirmed like any other change.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.RepoPath
			if path == "" {
				return exitcode.Errorf(exitcode.Usage,
					"no boot configuration repository is configured; set services.pxesrv.repoPath")
			}
			target, err := a.Role(role)
			if err != nil {
				return err
			}
			// What the pull brings is what every node gets at its next
			// network boot, so it is asked for like any other change.
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "update the boot configurations from version control on",
				Targets: singleNode(target.Host),
				Detail:  fmt.Sprintf("git -C %s pull --ff-only, on the %s host", path, role),
				// The target is the PXE service's host, not a node.
				NotNodes: true,
			}); err != nil {
				return err
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: []string{"git", "-C", path, "pull", "--ff-only"},
			})
			if err != nil {
				return err
			}
			return printLines(a, cmd, result.Output())
		}))
}

// maxLogLines bounds boot log --lines and dhcp log --lines.
const maxLogLines = 10000

func newBootLogCommand(r *root) *cobra.Command {
	var lines int
	cmd := leaf("log", "Show the PXE service log", `
Read the log of the PXE service, which is what to look at when a node asks
for a boot configuration and does not get the expected one.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			// The log is read into memory whole, on the way to the terminal
			// or an agent, so the count is bounded.
			if lines < 1 || lines > maxLogLines {
				return exitcode.Errorf(exitcode.Usage, "--lines is %d; give between 1 and %d", lines, maxLogLines)
			}
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.LogPath
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: []string{"tail", "-n", fmt.Sprint(lines), path},
			})
			if err != nil {
				return err
			}
			return printLines(a, cmd, result.Output())
		})
	cmd.Flags().IntVarP(&lines, "lines", "l", 50, fmt.Sprintf("how many log lines to show, at most %d", maxLogLines))
	return cmd
}

func newBootShellCommand(r *root) *cobra.Command {
	return leaf("shell [-- COMMAND...]", "Open a shell on the PXE service host", `
Log in to the host running the PXE service, or run one command on it.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			return roleShell(r, cmd, args, func(a *app.App) string { return a.Spec.Services.PXESrv.Role })
		})
}

func newBootGrubCommand(r *root) *cobra.Command {
	set := leaf("set NODE TARGET", "Point a node's GRUB configuration at a target", `
Link the GRUB configuration a node loads over TFTP to an installation target.

GRUB looks for a file named after the node's address in hexadecimal, which is
what this command computes. Unlike a PXE boot path, the link has no one-shot
form: GRUB loads the target at every boot until "boot grub unset" removes it.

TARGET is a file on the TFTP host, relative to services.tftp.grubPath unless
it is absolute. It has to exist and to lie under services.tftp.root, which is
all the TFTP server serves. The link is written relative to its directory, so
that a TFTP server confined to its root follows it too.

  clusterctl boot grub set exe0001 /srv/tftp/grub/1.0/grub.cfg.install-exec`,
		cobra.ExactArgs(2),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, node, link, err := grubLink(a, args[0])
			if err != nil {
				return err
			}
			target, rel, err := grubTarget(a, args[1])
			if err != nil {
				return err
			}
			// The target is looked for the way the link will find it, from
			// the link's directory, so a dry run refuses what the real run
			// would leave dangling.
			if _, err := a.ReadOnRole(a.Context(), role, transport.Request{
				Argv: []string{"test", "-f", path.Dir(link) + "/" + rel},
			}); err != nil {
				if exitcode.From(err) != exitcode.TargetFailed {
					return err
				}
				return exitcode.Errorf(exitcode.Usage, "%s is not a file on the TFTP host, so GRUB would load nothing", target)
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "set the GRUB configuration of",
				Targets: singleNode(node),
				Detail: fmt.Sprintf("%s -> %s, persistently: it stays until \"clusterctl boot grub unset %s\"",
					link, rel, node),
			}); err != nil {
				return err
			}
			if _, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: []string{"ln", "-sfn", "--", rel, link},
			}); err != nil {
				return err
			}
			a.Printf("%s now loads %s at every boot, until \"clusterctl boot grub unset %s\"\n", node, target, node)
			return nil
		}))

	unset := leaf("unset NODE", "Remove a node's GRUB configuration link", `
Remove the link that points a node's GRUB configuration at a target, so GRUB
no longer finds a configuration named after the node.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, node, link, err := grubLink(a, args[0])
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "remove the GRUB configuration of",
				Targets: singleNode(node),
				Detail:  "removes " + link,
			}); err != nil {
				return err
			}
			if _, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: []string{"rm", "-f", "--", link},
			}); err != nil {
				return err
			}
			a.Printf("removed the GRUB configuration of %s\n", node)
			return nil
		}))

	show := leaf("show NODE", "Show the GRUB file name a node loads", `
Print the address of a node and the GRUB configuration file name it asks for,
which is the address in hexadecimal.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			address, err := nodeAddress(a, args[0])
			if err != nil {
				return err
			}
			hex, err := addressToHex(address)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			t := output.NewTable(output.Cols("NODE", "ADDRESS", "GRUB FILE")...)
			t.Add(args[0], address, "grub.cfg-"+hex)
			return a.Print(output.Result{Table: t, Object: map[string]string{
				"node": args[0], "address": address, "grubFile": "grub.cfg-" + hex,
			}})
		}))

	return group("grub", "Configure what a node loads over TFTP", `
GRUB asks the TFTP service for a configuration named after the node's address
in hexadecimal. These commands compute that name and set or remove the link.`, set, unset, show)
}

// grubLink resolves the TFTP role, the one node named and the GRUB link
// named after its address.
func grubLink(a *app.App, expr string) (role, node, link string, err error) {
	role = a.Spec.Services.TFTP.Role
	if role == "" {
		return "", "", "", exitcode.Errorf(exitcode.Usage, "no host role runs the TFTP service; set services.tftp.role")
	}
	grubPath := a.Spec.Services.TFTP.GrubPath
	ns, err := a.Select(expr)
	if err != nil {
		return "", "", "", err
	}
	if ns.Len() != 1 {
		return "", "", "", exitcode.Errorf(exitcode.Usage, "%s is %d nodes; name one", expr, ns.Len())
	}
	node = ns.Expand()[0]
	addresses, err := nodeAddresses(a, []string{node})
	if err != nil {
		return "", "", "", err
	}
	hex, err := addressToHex(addresses[0])
	if err != nil {
		return "", "", "", exitcode.Wrap(exitcode.Usage, err)
	}
	return role, node, grubPath + "/grub.cfg-" + hex, nil
}

// grubTarget resolves the TARGET of boot grub set, a path on the TFTP host,
// and returns it with the path the link holds, which is relative to the
// link's directory: a TFTP server confined to its root resolves an absolute
// link inside that root, where the path does not exist. The target has to
// lie under the root, which is all the server serves.
func grubTarget(a *app.App, target string) (abs, rel string, err error) {
	spec := a.Spec.Services.TFTP
	root, dir := path.Clean(spec.Root), path.Clean(spec.GrubPath)
	under := func(p string) bool { return root == "/" || strings.HasPrefix(p, root+"/") }
	switch {
	case !path.IsAbs(root):
		return "", "", exitcode.Errorf(exitcode.Usage, "services.tftp.root %q is not an absolute path", spec.Root)
	case !path.IsAbs(dir) || dir != root && !under(dir):
		return "", "", exitcode.Errorf(exitcode.Usage,
			"services.tftp.grubPath %q is not a directory under services.tftp.root %s", spec.GrubPath, root)
	}
	abs = path.Clean(target)
	if !path.IsAbs(abs) {
		abs = path.Join(dir, target)
	}
	if !under(abs) {
		return "", "", exitcode.Errorf(exitcode.Usage,
			"%s is outside the TFTP root %s, which is all the TFTP server serves", abs, root)
	}
	split := func(p string) []string { return strings.FieldsFunc(p, func(c rune) bool { return c == '/' }) }
	from, to := split(dir), split(abs)
	common := 0
	for common < len(from) && common < len(to) && from[common] == to[common] {
		common++
	}
	return abs, path.Join(append(slices.Repeat([]string{".."}, len(from)-common), to[common:]...)...), nil
}

func singleNode(node string) *nodeset.NodeSet {
	ns := nodeset.New()
	_ = ns.Add(node)
	return ns
}

// addressToHex renders an IPv4 address the way GRUB asks for it: eight upper
// case hexadecimal digits.
func addressToHex(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("%q is not an address", address)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("%q is not an IPv4 address, which is what GRUB asks for", address)
	}
	return fmt.Sprintf("%02X%02X%02X%02X", v4[0], v4[1], v4[2], v4[3]), nil
}
