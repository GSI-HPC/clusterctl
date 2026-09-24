// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newNodeCommand(r *root) *cobra.Command {
	return group("node", "Select nodes and read what is known about them", `
Work with the node inventory and the node set syntax: resolve an expression,
list what the inventory knows, and look up the host and service processor
names the naming rules produce.`,
		newNodeListCommand(r),
		newNodeDescribeCommand(r),
		newNodeSelectCommand(r),
		newNodeFQDNCommand(r),
		newNodeGroupsCommand(r),
		newNodeAttrsCommand(r),
		newNodeRackCommand(r),
		newNodeHardwareCommand(r),
	)
}

func newNodeListCommand(r *root) *cobra.Command {
	return leaf("list [NODESET]", "List the nodes the inventory knows", `
List the nodes of the inventory, optionally limited to a node set.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			nodes := a.Inventory.All()
			if len(args) > 0 || a.Format.Kind == output.FormatNodeset {
				ns, err := a.SelectOptional(strings.Join(args, ","))
				if err != nil {
					return err
				}
				if ns != nil {
					selected, unknown := a.Inventory.Select(ns)
					if len(unknown) > 0 {
						a.Printf("not in the inventory: %s\n", strings.Join(unknown, ", "))
					}
					nodes = selected
				}
			}

			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "CLASS"},
				output.Column{Name: "RACK"},
				output.Column{Name: "ADDRESS"},
				output.Column{Name: "VENDOR", Wide: true},
				output.Column{Name: "CID", Wide: true},
			)
			for _, n := range nodes {
				t.Add(n.Name, n.Attributes["class"], n.Rack, n.Address, n.Attributes["vendor"], n.CID)
			}
			t.Caption = fmt.Sprintf("%d nodes", len(nodes))
			return a.Print(output.Result{Table: t, Object: nodes})
		})
}

func newNodeDescribeCommand(r *root) *cobra.Command {
	return leaf("describe NODE", "Show everything known about one node", `
Print the inventory entry of a node together with the host name, the service
processor name and the groups it belongs to.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			name := args[0]
			node, ok := a.Inventory.Lookup(name)
			if !ok {
				node = &inventory.Node{Name: name}
				a.Printf("%s is not in the inventory; showing what the naming rules produce\n", name)
			}
			fqdn, err := a.Namer.FQDN(name)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			// A node whose service processor has no name is still
			// described; the note says why the field is missing.
			bmc, err := a.BMCHost(name)
			if err != nil {
				a.Printf("%v\n", err)
			}
			memberships, _ := a.Groups.GroupsOf(name)

			t := output.NewTable(output.Cols("FIELD", "VALUE")...)
			t.Add("name", node.Name)
			t.Add("host", fqdn)
			addIf(t, "bmc", bmc)
			addIf(t, "address", node.Address)
			addIf(t, "bmcAddress", node.BMCAddress)
			addIf(t, "rack", node.Rack)
			addIf(t, "level", node.Level)
			addIf(t, "cid", node.CID)
			addIf(t, "macs", strings.Join(node.MACs, ", "))
			addIf(t, "bootPath", node.BootPath)
			for _, key := range sortedMapKeys(node.Attributes) {
				t.Add("attribute."+key, node.Attributes[key])
			}
			for _, source := range sortedMapKeys(memberships) {
				t.Add("groups."+source, strings.Join(memberships[source], ", "))
			}

			return a.Print(output.Result{Table: t, Object: map[string]any{
				"node": node, "host": fqdn, "bmc": bmc, "groups": memberships,
			}})
		})
}

func newNodeSelectCommand(r *root) *cobra.Command {
	var (
		expand bool
		count  bool
	)
	cmd := leaf("select [EXPRESSION]", "Resolve a node set expression", `
Resolve a node set expression and print the result, folded by default.

This is the node set calculator: ranges, groups and the set operators are all
evaluated, strictly from left to right.

  clusterctl node select 'exe[1-10]!exe5'
  clusterctl node select '@idle&@rack:R02' --expand
  clusterctl node select '@exe' --count`,
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
			switch {
			case count:
				return say(cmd, "%d\n", ns.Len())
			case expand:
				for _, name := range ns.Expand() {
					if err := say(cmd, "%s\n", name); err != nil {
						return err
					}
				}
				return nil
			case a.Format.IsMachine():
				return a.Print(output.Result{Nodes: ns, Object: ns.Expand()})
			default:
				return say(cmd, "%s\n", ns)
			}
		})
	cmd.Flags().BoolVarP(&expand, "expand", "e", false, "print one node per line instead of folding")
	cmd.Flags().BoolVarP(&count, "count", "c", false, "print how many nodes the expression names")
	cmd.ValidArgsFunction = completeGroups(r)
	return cmd
}

func newNodeFQDNCommand(r *root) *cobra.Command {
	var bmc bool
	cmd := leaf("fqdn [NODESET]", "Print the host names of a node set", `
Apply the naming rules to a node set and print the result.

With --bmc the names of the service processors are printed instead, which is
what the out-of-band commands connect to.`,
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
			mapped := a.Namer.FQDNSet
			if bmc {
				mapped = a.BMCHosts
			}
			names, err := mapped(ns)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if a.Format.IsMachine() {
				return a.Print(output.Result{Nodes: names, Object: names.Expand()})
			}
			return say(cmd, "%s\n", names)
		})
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "print the service processor names")
	return cmd
}

func newNodeGroupsCommand(r *root) *cobra.Command {
	return leaf("groups [NODE]", "List the node groups, or the groups of one node", `
Without an argument, list every group source and the groups it offers. With a
node name, list the groups that node belongs to.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				memberships, err := a.Groups.GroupsOf(args[0])
				if err != nil {
					return err
				}
				t := output.NewTable(output.Cols("SOURCE", "GROUPS")...)
				for _, source := range sortedMapKeys(memberships) {
					t.Add(source, strings.Join(memberships[source], ", "))
				}
				return a.Print(output.Result{Table: t, Object: memberships})
			}

			t := output.NewTable(
				output.Column{Name: "SOURCE"},
				output.Column{Name: "GROUP"},
				output.Column{Name: "NODES", Wide: true},
			)
			listing := map[string][]string{}
			for _, source := range a.Groups.Sources() {
				names, err := a.Groups.List(source)
				if err != nil {
					t.Add(source, "", "cannot be listed: "+err.Error())
					continue
				}
				listing[source] = names
				for _, name := range names {
					expr, err := a.Groups.Resolve(source, name)
					if err != nil {
						t.Add(source, name, "error: "+err.Error())
						continue
					}
					t.Add(source, name, expr)
				}
			}
			return a.Print(output.Result{Table: t, Object: listing})
		})
}

func newNodeAttrsCommand(r *root) *cobra.Command {
	return leaf("attrs [KEY]", "List the node attributes and their values", `
List the attribute names the inventory carries, or the values of one
attribute together with the nodes that have them. This is what the genders
file used to answer.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				t := output.NewTable(output.Cols("ATTRIBUTE", "VALUES")...)
				object := map[string][]string{}
				for _, key := range a.Inventory.AttributeKeys() {
					values := a.Inventory.AttributeValues(key)
					object[key] = values
					t.Add(key, strings.Join(values, ", "))
				}
				return a.Print(output.Result{Table: t, Object: object})
			}

			key := args[0]
			t := output.NewTable(output.Cols("VALUE", "NODES", "COUNT")...)
			object := map[string]string{}
			for _, value := range a.Inventory.AttributeValues(key) {
				ns := a.Inventory.WithAttribute(key, value)
				object[value] = ns.String()
				t.Add(value, ns.String(), fmt.Sprint(ns.Len()))
			}
			if t.Len() == 0 {
				return exitcode.Errorf(exitcode.Usage, "no node carries the attribute %q", key)
			}
			return a.Print(output.Result{Table: t, Object: object})
		})
}

func newNodeRackCommand(r *root) *cobra.Command {
	return leaf("rack [RACK|NODE]", "List racks and what they hold", `
Without an argument, list every rack with the nodes in it. With a rack name,
list that rack; with a node name, list the rack that node sits in.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			racks := a.Inventory.Racks()
			if len(args) == 1 {
				wanted := args[0]
				if node, ok := a.Inventory.Lookup(wanted); ok && node.Rack != "" {
					wanted = node.Rack
				}
				found := false
				for _, rack := range racks {
					if strings.EqualFold(rack, wanted) {
						racks, found = []string{rack}, true
						break
					}
				}
				if !found {
					return exitcode.Errorf(exitcode.Usage, "no rack or node named %q is in the inventory", args[0])
				}
			}

			t := output.NewTable(
				output.Column{Name: "RACK"},
				output.Column{Name: "NODES"},
				output.Column{Name: "COUNT", Right: true},
			)
			object := map[string]string{}
			for _, rack := range racks {
				ns := a.Inventory.InRack(rack)
				object[rack] = ns.String()
				t.Add(rack, ns.String(), fmt.Sprint(ns.Len()))
			}
			return a.Print(output.Result{Table: t, Object: object})
		})
}

// hardwareScript reads the hardware identification a node exposes through
// sysfs and lspci. It is one script so that one connection answers
// everything.
const hardwareScript = `
set -u
read_first() { for f in "$@"; do [ -r "$f" ] && { tr -d '\n' < "$f"; return; }; done; }
printf '%s|' "$(read_first /sys/class/dmi/id/sys_vendor)"
printf '%s|' "$(read_first /sys/class/dmi/id/product_name)"
printf '%s|' "$(read_first /sys/class/dmi/id/board_vendor)"
printf '%s|' "$(read_first /sys/class/dmi/id/board_name)"
printf '%s|' "$(read_first /sys/class/dmi/id/bios_vendor)"
printf '%s|' "$(read_first /sys/class/dmi/id/bios_version)"
printf '%s|' "$(read_first /sys/class/dmi/id/bios_date)"
printf '%s' "$(lspci 2>/dev/null | grep -o -e 'MT[0-9]*' -e '\[ConnectX[^]]*\]' | tr '\n' ' ')"
`

func newNodeHardwareCommand(r *root) *cobra.Command {
	return leaf("hw [NODESET]", "Collect the hardware inventory of a node set", `
Ask each node what hardware it is: system and board vendor, BIOS version and
date, and the InfiniBand adapters it carries.

One script per node answers everything, so a node is contacted once.`,
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
			results, err := runOnNodes(a, ns, func(string) transport.Request {
				return transport.Request{
					Script:  hardwareScript,
					Timeout: a.Timeout().Get(),
					TTY:     transport.TTYNone,
				}
			})
			if err != nil {
				return err
			}

			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "VENDOR"},
				output.Column{Name: "PRODUCT"},
				output.Column{Name: "BOARD", Wide: true},
				output.Column{Name: "BIOS"},
				output.Column{Name: "BIOS DATE", Wide: true},
				output.Column{Name: "INFINIBAND"},
			)
			object := make([]map[string]string, 0, len(results))
			for _, res := range results {
				if res.Failed() {
					t.Add(res.Target.Name, "unreachable", "", "", "", "", "")
					continue
				}
				f := strings.Split(res.Output(), "|")
				for len(f) < 8 {
					f = append(f, "")
				}
				t.Add(res.Target.Name, f[0], f[1], f[2]+" "+f[3], f[4]+" "+f[5], f[6], strings.TrimSpace(f[7]))
				object = append(object, map[string]string{
					"node": res.Target.Name, "vendor": f[0], "product": f[1],
					"boardVendor": f[2], "boardName": f[3],
					"biosVendor": f[4], "biosVersion": f[5], "biosDate": f[6],
					"infiniband": strings.TrimSpace(f[7]),
				})
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			return failureError(results)
		})
}

func addIf(t *output.Table, field, value string) {
	if value != "" {
		t.Add(field, value)
	}
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
