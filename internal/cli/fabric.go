// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
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

// nodeGUIDs derives the adapter identifiers of a node from the hardware
// addresses the inventory or DHCP knows.
func nodeGUIDs(a *app.App, node string) ([]string, error) {
	var macs []string
	if entry, ok := a.Inventory.Lookup(node); ok {
		macs = append(macs, entry.MACs...)
	}
	if len(macs) == 0 {
		cfg, err := dhcpConfig(a)
		if err != nil {
			return nil, err
		}
		for _, host := range cfg.Lookup(node) {
			macs = append(macs, host.MACs...)
		}
	}
	if len(macs) == 0 {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no hardware address is known for %s; set it in the inventory or in DHCP", node)
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
Print the adapter identifier of each node, derived from its hardware address.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("NODE", "GUID")...)
			object := map[string][]string{}
			for _, node := range ns.Expand() {
				guids, err := nodeGUIDs(a, node)
				if err != nil {
					t.Add(node, err.Error())
					continue
				}
				object[node] = guids
				for _, guid := range guids {
					t.Add(node, guid)
				}
			}
			return a.Print(output.Result{Table: t, Object: object})
		})
}

func newFabricStateCommand(r *root) *cobra.Command {
	return leaf("state [NODESET]", "Check whether the fabric ports of a node set are up", `
Ask the fabric about each node's port.

  clusterctl fabric state -n exe[1-10]`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := fabricRole(a)
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}

			type entry struct{ node, guid string }
			var entries []entry
			for _, node := range ns.Expand() {
				guids, err := nodeGUIDs(a, node)
				if err != nil {
					return err
				}
				for _, guid := range guids {
					entries = append(entries, entry{node: node, guid: guid})
				}
			}

			// One script asks about every port, rather than one connection
			// to the fabric host per node.
			var script strings.Builder
			script.WriteString("set -u\n")
			for _, e := range entries {
				fmt.Fprintf(&script,
					"printf '%%s %%s ' %s %s; ibportstate -G %s query 2>/dev/null | "+
						"awk '/PhysLinkState|LinkState|LinkWidth|LinkSpeed/{printf \"%%s \", $0}' || true; echo\n",
					e.node, e.guid, e.guid)
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Script:  script.String(),
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}

			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "GUID"},
				output.Column{Name: "STATE"},
				output.Column{Name: "DETAIL", Wide: true},
			)
			down := 0
			for _, line := range result.Lines() {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					continue
				}
				detail := strings.Join(fields[2:], " ")
				state := "down"
				if strings.Contains(detail, "Active") || strings.Contains(detail, "LinkUp") {
					state = "up"
				} else {
					down++
				}
				t.Add(fields[0], fields[1], state, detail)
			}
			if err := a.Print(output.Result{Table: t}); err != nil {
				return err
			}
			if down > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d ports are not up", down)
			}
			return nil
		})
}

func newFabricCountersCommand(r *root) *cobra.Command {
	var uplink bool
	cmd := leaf("counters NODE", "Show the fabric error counters of a node", `
Read the error counters of a node's port, or of the switch port it is
connected to.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := fabricRole(a)
			if err != nil {
				return err
			}
			guids, err := nodeGUIDs(a, args[0])
			if err != nil {
				return err
			}
			argv := []string{"ibqueryerrors", "-G", guids[0], "--data"}
			if uplink {
				argv = []string{"ibqueryerrors", "--switch", "--verbose", "--data", "--details", "--report-port"}
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    argv,
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Output())
			return nil
		})
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
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{Script: script, Timeout: a.Timeout().Get(), TTY: transport.TTYNone}
			})
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
		})
}

func newHCACableCommand(r *root) *cobra.Command {
	return leaf("cable [NODESET]", "Show the cable in each node's adapter", `
Report the cable part number and length each node's adapter sees.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{Script: script, Timeout: a.Timeout().Get(), TTY: transport.TTYNone}
			})
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
		})
}

func newHCAConfigCommand(r *root) *cobra.Command {
	return leaf("config KEY [VALUE] [NODESET]", "Read or set an adapter firmware setting", `
Read a firmware setting from each node's adapter, or set it.

Setting a firmware value changes hardware behaviour across a reboot, so it
goes through the confirmation gate.

  clusterctl hca config KEEP_LINK_UP_ON_BOOT_P1 -n exe[1-4]
  clusterctl hca config KEEP_LINK_UP_ON_BOOT_P1 1 -n exe[1-4]`,
		cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			key := args[0]
			value := ""
			rest := args[1:]
			if len(rest) > 0 && !strings.ContainsAny(rest[0], "[@,") {
				value, rest = rest[0], rest[1:]
			}
			ns, err := selection(a, rest)
			if err != nil {
				return err
			}

			if value == "" {
				results, err := runOnNodes(a, ns, func(string) transport.Request {
					return transport.Request{
						Argv:    []string{"sh", "-c", "mlxconfig -e query 2>/dev/null | grep -- " + shellQuote(key)},
						Timeout: a.Timeout().Get(),
						TTY:     transport.TTYNone,
					}
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
			}

			if err := a.Gate.Confirm(safetyAction("change the adapter firmware setting of", ns,
				fmt.Sprintf("%s=%s", key, value))); err != nil {
				return dryRunOrError(err)
			}
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Script:  fmt.Sprintf("set -eu\ndev=$(ibstat -l | head -n1)\nmlxconfig --yes --dev \"$dev\" set %s=%s\n", key, value),
					Timeout: a.Timeout().Get(),
					TTY:     transport.TTYNone,
				}
			})
			if err != nil {
				return err
			}
			if err := a.Print(output.Result{Table: resultsTable(results), Object: results}); err != nil {
				return err
			}
			return failureError(results)
		})
}

func newHCAFirmwareCommand(r *root) *cobra.Command {
	return leaf("firmware [NODESET]", "Show the adapter firmware version of a node set", `
Report the adapter firmware version each node is running.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{Script: script, Timeout: a.Timeout().Get(), TTY: transport.TTYNone}
			})
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
		})
}
