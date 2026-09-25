// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func newFabricCommand(r *root) *cobra.Command {
	return group("fabric", "Check the InfiniBand fabric", `
Ask the fabric whether a node's port is up and read its error counters.

A node that has not booted yet cannot be asked, so its port is identified by
the adapter identifier derived from the hardware address DHCP knows. That is
what makes it possible to see whether a machine's link came up before the
machine did.`,
		newFabricGUIDCommand(r),
		newFabricStateCommand(r),
		newFabricCountersCommand(r),
	)
}

// guidLookup derives the adapter identifiers of nodes from the hardware
// addresses DHCP or the inventory knows.
//
// DHCP comes first, because it is what the node boots with and what the
// documentation promises; the inventory answers only for a node DHCP does
// not know, or when no host role runs the DHCP server. The DHCP
// configuration is read once, and a failure to read it fails the lookup
// rather than quietly falling back to an inventory address that may be stale.
type guidLookup struct {
	a      *app.App
	loaded bool
	dhcp   *dhcp.Config
	err    error
}

func newGUIDLookup(a *app.App) *guidLookup { return &guidLookup{a: a} }

// load reads the DHCP configuration, once.
func (l *guidLookup) load() error {
	if l.loaded {
		return l.err
	}
	l.loaded = true
	if l.a.Spec.Services.DHCP.Role == "" {
		return nil
	}
	cfg, err := dhcpConfig(l.a)
	if err != nil {
		l.err = fmt.Errorf("reading the DHCP configuration: %w", err)
		return l.err
	}
	l.dhcp = cfg
	return nil
}

// guids returns the adapter identifiers of one node.
func (l *guidLookup) guids(node string) ([]string, error) {
	if err := l.load(); err != nil {
		return nil, err
	}
	var macs []string
	if l.dhcp != nil {
		for _, host := range l.dhcp.Lookup(node) {
			macs = append(macs, host.MACs...)
		}
	}
	if len(macs) == 0 {
		if entry, ok := l.a.Inventory.Lookup(node); ok {
			macs = append(macs, entry.MACs...)
		}
	}
	if len(macs) == 0 {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no hardware address is known for %s; set it in DHCP or in the inventory", node)
	}

	out := make([]string, 0, len(macs))
	for _, mac := range macs {
		guid, err := dhcp.GUIDFromMAC(mac)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Usage, err)
		}
		out = append(out, guid)
	}
	return out, nil
}

func fabricRole(a *app.App) (string, error) {
	role := a.Spec.Services.Fabric.Role
	if role == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no host role can reach the fabric; set services.fabric.role")
	}
	return role, nil
}

func newFabricGUIDCommand(r *root) *cobra.Command {
	return leaf("guid [NODESET]", "Show the fabric identifiers of a node set", `
Print the adapter identifier of each node, derived from its hardware address.

The hardware address comes from DHCP, and from the inventory for a node DHCP
does not know. A node whose identifier cannot be derived is named on standard
error and makes the command fail; it is left out of the table and of -o json.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			lookup := newGUIDLookup(a)
			// A DHCP server that cannot be read fails every node the same
			// way, so it fails the command rather than each row.
			if err := lookup.load(); err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "GUID")...)
			object := map[string][]string{}
			failed := nodeset.New()
			for _, node := range ns.Expand() {
				guids, err := lookup.guids(node)
				if err != nil {
					a.Printf("clusterctl: %s: %s\n", node, output.EscapeCell(err.Error()))
					_ = failed.Add(node)
					continue
				}
				object[node] = guids
				for _, guid := range guids {
					t.Add(node, guid)
				}
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if !failed.IsEmpty() {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d nodes have no fabric identifier: %s",
					failed.Len(), ns.Len(), failed)
			}
			return nil
		}))
}

// portState is what the fabric says about one port.
type portState struct {
	Node string `json:"node" yaml:"node"`
	GUID string `json:"guid" yaml:"guid"`
	// State is up only when the logical link state is Active.
	State         string `json:"state" yaml:"state"`
	LinkState     string `json:"linkState" yaml:"linkState"`
	PhysicalState string `json:"physicalState" yaml:"physicalState"`
	Width         string `json:"width,omitempty" yaml:"width,omitempty"`
	Speed         string `json:"speed,omitempty" yaml:"speed,omitempty"`
}

// The states fabric state reports.
const (
	portUp       = "up"
	portDown     = "down"
	portNoAnswer = "no answer"
)

// portStateScript builds the one script that asks about every port, rather
// than opening one connection to the fabric host per node.
//
// Each answer line starts with the index of its port instead of the node
// name: a node name comes from the inventory or from dhcpd.conf, and nothing
// from there belongs in a script that runs as root on the fabric host. The
// identifiers are hexadecimal by construction and quoted all the same.
//
// ibportstate takes the port number after the destination; 1 is the port of
// an adapter function, which is what a port identifier derived from a
// hardware address names.
func portStateScript(guids []string) string {
	var script strings.Builder
	script.WriteString(`set -u
query() {
  ibportstate -G "$1" 1 query 2>/dev/null | awk '
    { key = $0; sub(/:.*/, "", key)
      value = $0; sub(/^[^:]*:\.*/, "", value)
      if (!(key in seen)) seen[key] = value }
    END { printf "%s|%s|%s|%s", seen["LinkState"], seen["PhysLinkState"], seen["LinkWidthActive"], seen["LinkSpeedActive"] }'
}
`)
	for i, guid := range guids {
		fmt.Fprintf(&script, "printf '%d|'; query %s; echo\n", i, shellQuote(guid))
	}
	return script.String()
}

// linkStateValue strips the numeric prefix some versions of ibportstate put
// before the state name.
var linkStateValue = regexp.MustCompile(`^\d+:\s*`)

// parsePortStates reads the answers of portStateScript into the entries
// they belong to. A port the fabric did not answer for is reported as such.
func parsePortStates(entries []portState, out string) {
	for i := range entries {
		entries[i].State = portNoAnswer
	}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "|")
		if len(f) != 5 {
			continue
		}
		i, err := strconv.Atoi(f[0])
		if err != nil || i < 0 || i >= len(entries) {
			continue
		}
		e := &entries[i]
		e.LinkState = linkStateValue.ReplaceAllString(strings.TrimSpace(f[1]), "")
		e.PhysicalState = linkStateValue.ReplaceAllString(strings.TrimSpace(f[2]), "")
		e.Width = strings.TrimSpace(f[3])
		e.Speed = strings.TrimSpace(f[4])
		switch {
		case e.LinkState == "" && e.PhysicalState == "":
			e.State = portNoAnswer
		case e.LinkState == "Active":
			e.State = portUp
		default:
			e.State = portDown
		}
	}
}

func newFabricStateCommand(r *root) *cobra.Command {
	return leaf("state [NODESET]", "Check whether the fabric ports of a node set are up", `
Ask the fabric about each node's port.

A port is up only when its logical link state is Active. A port that is
physically linked but still Initialize or Armed has no subnet manager
configuration yet and carries no traffic, so it is reported down, with both
states shown. A port the fabric does not answer for is reported as such. Any
port that is not up makes the command fail.

  clusterctl fabric state -n exe[1-10]`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := fabricRole(a)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}

			lookup := newGUIDLookup(a)
			var entries []portState
			var guids []string
			for _, node := range ns.Expand() {
				nodeGUIDs, err := lookup.guids(node)
				if err != nil {
					return err
				}
				for _, guid := range nodeGUIDs {
					entries = append(entries, portState{Node: node, GUID: guid})
					guids = append(guids, guid)
				}
			}

			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Script: portStateScript(guids),
			})
			if err != nil {
				return err
			}
			parsePortStates(entries, result.Stdout)

			t := output.NewTable(output.Cols("NODE", "GUID", "STATE", "LINK", "PHYSICAL", "WIDTH", "SPEED").
				Wide("WIDTH", "SPEED")...)
			notUp := 0
			for _, e := range entries {
				if e.State != portUp {
					notUp++
				}
				t.Add(e.Node, e.GUID, e.State, e.LinkState, e.PhysicalState, e.Width, e.Speed)
			}
			if err := a.Print(output.Result{Table: t, Object: entries}); err != nil {
				return err
			}
			if notUp > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d ports are not up", notUp, len(entries))
			}
			return nil
		}))
}

// switchPort is the switch port a node's port is cabled to.
type switchPort struct {
	GUID string
	LID  int
	Port int
}

var (
	// ibaddr prints "GID fe80::... LID start 0x5 end 0x5".
	ibaddrLID = regexp.MustCompile(`LID start (0x[0-9a-fA-F]+|[0-9]+)`)
	// iblinkinfo --line prints one link per line, the switch port first:
	//   0x0002c90200404ad8 "switch name"  4   17[  ] ==( 4X 25.78 Gbps Active/  LinkUp)==>  0x0002c903000e0b72  5  1[  ] "node HCA-1" ( )
	iblinkinfoLine = regexp.MustCompile(
		`^\s*(0x[0-9a-fA-F]+)\s+"[^"]*"\s+([0-9]+)\s+([0-9]+)\[[^\]]*\]\s+==\(.*\)==>\s+(0x[0-9a-fA-F]+)\s+([0-9]+)\s+([0-9]+)\[`)
)

// portLID asks the subnet manager for the LID of a port.
func portLID(a *app.App, role, guid string) (int, error) {
	result, err := a.RunOnRole(a.Context(), role, transport.Request{
		Argv: []string{"ibaddr", "-G", guid},
	})
	if err != nil {
		return 0, err
	}
	m := ibaddrLID.FindStringSubmatch(result.Stdout)
	if m == nil {
		return 0, exitcode.Errorf(exitcode.TargetFailed,
			"the fabric has no LID for port %s; is its link up?", guid)
	}
	lid, err := strconv.ParseInt(m[1], 0, 32)
	if err != nil || lid <= 0 {
		return 0, exitcode.Errorf(exitcode.TargetFailed, "ibaddr reported LID %q for port %s", m[1], guid)
	}
	return int(lid), nil
}

// findUplink picks the one switch port whose peer is the port with the given
// LID out of what iblinkinfo --line printed.
func findUplink(out string, lid int) (switchPort, error) {
	var found []switchPort
	for line := range strings.SplitSeq(out, "\n") {
		m := iblinkinfoLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if peer, err := strconv.Atoi(m[5]); err != nil || peer != lid {
			continue
		}
		swLID, err1 := strconv.Atoi(m[2])
		port, err2 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil {
			continue
		}
		found = append(found, switchPort{GUID: m[1], LID: swLID, Port: port})
	}
	switch len(found) {
	case 0:
		return switchPort{}, exitcode.Errorf(exitcode.TargetFailed,
			"no switch port is linked to LID %d; is the link up?", lid)
	case 1:
		return found[0], nil
	default:
		return switchPort{}, exitcode.Errorf(exitcode.TargetFailed,
			"%d switch ports claim to be linked to LID %d; refusing to guess", len(found), lid)
	}
}

func newFabricCountersCommand(r *root) *cobra.Command {
	var uplink bool
	cmd := leaf("counters NODE", "Show the fabric error counters of a node", `
Read the error counters of a node's port, or of the switch port it is
connected to.

With --uplink the switch port is found by asking the subnet manager for the
LID of the node's port and looking for the one switch port linked to it, and
only that port is read. The switch and port are named on standard error.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			role, err := fabricRole(a)
			if err != nil {
				return err
			}
			guids, err := newGUIDLookup(a).guids(args[0])
			if err != nil {
				return err
			}
			argv := []string{"ibqueryerrors", "-G", guids[0], "--data"}
			if uplink {
				lid, err := portLID(a, role, guids[0])
				if err != nil {
					return err
				}
				links, err := a.RunOnRole(a.Context(), role, transport.Request{
					Argv: []string{"iblinkinfo", "--line"},
				})
				if err != nil {
					return err
				}
				sw, err := findUplink(links.Stdout, lid)
				if err != nil {
					return fmt.Errorf("%s: %w", args[0], err)
				}
				a.Printf("%s is linked to port %d of switch %s (LID %d)\n", args[0], sw.Port, sw.GUID, sw.LID)
				argv = []string{"perfquery", strconv.Itoa(sw.LID), strconv.Itoa(sw.Port)}
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv: argv,
			})
			if err != nil {
				return err
			}
			return say(cmd, "%s\n", output.EscapeText(result.Output()))
		}))
	cmd.Flags().BoolVar(&uplink, "uplink", false, "read the switch port instead of the node port")
	return cmd
}

func newHCACommand(r *root) *cobra.Command {
	return group("hca", "Work with the host channel adapters of the nodes", `
Read and change the adapter firmware settings of the nodes themselves. These
commands need the vendor tools on the node, so the node has to be up.`,
		newHCALinkCommand(r),
		newHCACableCommand(r),
		newHCAConfigCommand(r),
		newHCAFirmwareCommand(r),
	)
}

func newHCALinkCommand(r *root) *cobra.Command {
	return leaf("link [NODESET]", "Show the adapter link state of a node set", `
Report the state, rate and physical state of each node's adapter.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			const script = `
set -u
for dev in $(ibstat -l 2>/dev/null); do
  state=$(ibstat "$dev" 1 2>/dev/null | awk -F': *' '/State:/{print $2}')
  phys=$(ibstat "$dev" 1 2>/dev/null | awk -F': *' '/Physical state:/{print $2}')
  rate=$(ibstat "$dev" 1 2>/dev/null | awk -F': *' '/Rate:/{print $2}')
  printf '%s|%s|%s|%s\n' "$dev" "$state" "$phys" "$rate"
done
`
			results, err := runOnNodes(a, ns, transport.Request{Script: script})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "DEVICE", "STATE", "PHYSICAL", "RATE")...)
			for _, res := range results {
				if res.Failed() {
					t.Add(res.Target.Name, "unreachable", "", "", "")
					continue
				}
				for _, line := range res.Lines() {
					f := strings.SplitN(line, "|", 4)
					for len(f) < 4 {
						f = append(f, "")
					}
					t.Add(res.Target.Name, f[0], f[1], f[2], f[3])
				}
			}
			if err := a.Print(output.Result{Table: t, Object: results}); err != nil {
				return err
			}
			return failureError(results)
		}))
}

func newHCACableCommand(r *root) *cobra.Command {
	return leaf("cable [NODESET]", "Show the cable in each node's adapter", `
Report the cable part number and length each node's adapter sees.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			const script = `
set -u
mst cable add >/dev/null 2>&1 || true
mlxcables -q 2>/dev/null | awk -F': *' '
  /^Part number/ {part=$2}
  /^Length/      {len=$2}
  END           {printf "%s|%s\n", part, len}'
`
			results, err := runOnNodes(a, ns, transport.Request{Script: script})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "PART NUMBER", "LENGTH")...)
			for _, res := range results {
				if res.Failed() {
					t.Add(res.Target.Name, "unreachable", "")
					continue
				}
				f := strings.SplitN(res.Output(), "|", 2)
				for len(f) < 2 {
					f = append(f, "")
				}
				t.Add(res.Target.Name, strings.TrimSpace(f[0]), strings.TrimSpace(f[1]))
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			return failureError(results)
		}))
}

func newHCAConfigCommand(r *root) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read or set an adapter firmware setting",
		Long: strings.TrimSpace(`
Read a firmware setting from the adapters of each node, or set it on every
adapter of each node.

Reading and writing are separate commands, so that a node set can never be
taken for the value to write.`),
		// The form this command used to take, hca config KEY [VALUE], would
		// otherwise print the help and exit 0 as if it had done something.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitcode.Errorf(exitcode.Usage,
					"unknown command %q: read a setting with \"hca config get KEY\" and set one with \"hca config set KEY VALUE\"",
					args[0])
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newHCAConfigGetCommand(r), newHCAConfigSetCommand(r))
	return cmd
}

var (
	// mlxconfigKey is the shape of an mlxconfig parameter name, with the
	// optional index some parameters take, such as MODULE_SPLIT_M0[1..3].
	mlxconfigKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\[[0-9]+(\.\.[0-9]+)?\])?$`)
	// mlxconfigValue is the shape of an mlxconfig value: a number, a
	// hexadecimal number or a symbolic name such as True or ETH.
	mlxconfigValue = regexp.MustCompile(`^[A-Za-z0-9_.:+-]+$`)
)

// checkMlxconfig refuses a key or value mlxconfig would not take, before
// anything is sent. Both are quoted all the same.
func checkMlxconfig(key, value string, withValue bool) error {
	if !mlxconfigKey.MatchString(key) {
		return exitcode.Errorf(exitcode.Usage,
			"%q is not an adapter firmware setting; it has letters, digits and underscores, and an optional [N] or [N..M]", key)
	}
	if withValue && !mlxconfigValue.MatchString(value) {
		return exitcode.Errorf(exitcode.Usage,
			"%q is not an adapter firmware value; it has letters, digits and . : _ + -", value)
	}
	return nil
}

func newHCAConfigGetCommand(r *root) *cobra.Command {
	return leaf("get KEY [NODESET]", "Read an adapter firmware setting", `
Read a firmware setting from the adapters of each node.

  clusterctl hca config get KEEP_LINK_UP_ON_BOOT_P1 -n exe[1-4]`,
		cobra.MinimumNArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			key := args[0]
			if err := checkMlxconfig(key, "", false); err != nil {
				return err
			}
			ns, err := selection(a, args[1:])
			if err != nil {
				return err
			}
			results, err := runOnNodes(a, ns, transport.Request{
				Argv: []string{"sh", "-c", "mlxconfig -e query 2>/dev/null | grep -F -- " + shellQuote(key)},
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", key)...)
			for _, res := range results {
				t.Add(res.Target.Name, strings.Join(strings.Fields(res.Output()), " "))
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			return failureError(results)
		}))
}

// hcaSetScript sets one firmware value on every adapter of a node and prints
// one line per adapter, so that a node with two adapters is not reported
// done when only the first was changed.
func hcaSetScript(key, value string) string {
	return fmt.Sprintf(`set -u
devs=$(ibstat -l 2>/dev/null) || devs=
if [ -z "$devs" ]; then
  echo "no adapter found" >&2
  exit 1
fi
rc=0
for dev in $devs; do
  if out=$(mlxconfig --yes --dev "$dev" set %s 2>&1); then
    printf '%%s|ok|\n' "$dev"
  else
    printf '%%s|failed|%%s\n' "$dev" "$(printf '%%s\n' "$out" | tail -n 1)"
    rc=1
  fi
done
exit $rc
`, shellQuote(key+"="+value))
}

func newHCAConfigSetCommand(r *root) *cobra.Command {
	return leaf("set KEY VALUE [NODESET]", "Set an adapter firmware setting", `
Set a firmware setting on every adapter of each node, and report each
adapter. A node where any adapter was not changed fails the command.

Setting a firmware value changes hardware behaviour across a reboot, so it
goes through the confirmation gate.

  clusterctl hca config set KEEP_LINK_UP_ON_BOOT_P1 1 -n exe[1-4]`,
		cobra.MinimumNArgs(2),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			if err := checkMlxconfig(key, value, true); err != nil {
				return err
			}
			ns, err := selection(a, args[2:])
			if err != nil {
				return err
			}

			if err := a.Gate.Confirm(safetyAction("change the adapter firmware setting of", ns,
				fmt.Sprintf("%s=%s on every adapter", key, value))); err != nil {
				return err
			}
			results, err := runOnNodes(a, ns, transport.Request{
				Script: hcaSetScript(key, value),
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "DEVICE", "STATUS", "ERROR").Wide("ERROR")...)
			for _, res := range results {
				lines := res.Lines()
				if res.Err != nil || len(lines) == 0 {
					status := "failed"
					detail := strings.TrimSpace(lastNonEmpty(res.Stderr))
					if res.Err != nil {
						status, detail = "unreachable", res.Err.Error()
					}
					t.Add(res.Target.Name, "", status, detail)
					continue
				}
				for _, line := range lines {
					f := strings.SplitN(line, "|", 3)
					for len(f) < 3 {
						f = append(f, "")
					}
					t.Add(res.Target.Name, f[0], f[1], f[2])
				}
			}
			if err := a.Print(output.Result{Table: t, Object: results}); err != nil {
				return err
			}
			return failureError(results)
		}))
}

func newHCAFirmwareCommand(r *root) *cobra.Command {
	return leaf("firmware [NODESET]", "Show the adapter firmware version of a node set", `
Report the adapter firmware version each node is running.`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			const script = `
set -u
for dev in $(ibstat -l 2>/dev/null); do
  fw=$(ibstat "$dev" 2>/dev/null | awk -F': *' '/Firmware version:/{print $2}')
  printf '%s|%s\n' "$dev" "$fw"
done
`
			results, err := runOnNodes(a, ns, transport.Request{Script: script})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "DEVICE", "FIRMWARE")...)
			for _, res := range results {
				if res.Failed() {
					t.Add(res.Target.Name, "unreachable", "")
					continue
				}
				for _, line := range res.Lines() {
					f := strings.SplitN(line, "|", 2)
					for len(f) < 2 {
						f = append(f, "")
					}
					t.Add(res.Target.Name, f[0], f[1])
				}
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			return failureError(results)
		}))
}
