// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package inventory answers questions about the nodes of a site: their
// attributes, where they sit, what address they have and what they boot.
//
// It replaces the genders file, the rack spreadsheet and the boot path table
// the shell tools read separately.
package inventory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Node is everything the inventory knows about one machine.
type Node struct {
	Name       string            `json:"name" yaml:"name"`
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Rack       string            `json:"rack,omitempty" yaml:"rack,omitempty"`
	Level      string            `json:"level,omitempty" yaml:"level,omitempty"`
	Address    string            `json:"address,omitempty" yaml:"address,omitempty"`
	BMCAddress string            `json:"bmcAddress,omitempty" yaml:"bmcAddress,omitempty"`
	CID        string            `json:"cid,omitempty" yaml:"cid,omitempty"`
	MACs       []string          `json:"macs,omitempty" yaml:"macs,omitempty"`
	BootPath   string            `json:"bootPath,omitempty" yaml:"bootPath,omitempty"`
}

// Attribute returns one attribute of the node.
func (n Node) Attribute(key string) (string, bool) {
	v, ok := n.Attributes[key]
	return v, ok
}

// Inventory holds the nodes of a site, indexed by name.
type Inventory struct {
	nodes map[string]*Node
	order []string
}

// New builds an inventory from the NodeInventory documents of a site.
//
// Entries are applied in order, so a general entry may be written first and
// refined by a later one naming fewer nodes. Fields that describe a single
// machine may only be set by an entry that names exactly one node.
func New(specs ...v1alpha1.NodeInventorySpec) (*Inventory, error) {
	inv := &Inventory{nodes: map[string]*Node{}}
	for _, spec := range specs {
		for i, entry := range spec.Nodes {
			ns, err := nodeset.Parse(entry.Nodes)
			if err != nil {
				return nil, fmt.Errorf("inventory entry %d: %w", i+1, err)
			}
			if ns.IsEmpty() {
				return nil, fmt.Errorf("inventory entry %d names no node", i+1)
			}
			if ns.Len() > 1 {
				if field := singleNodeField(entry); field != "" {
					return nil, fmt.Errorf(
						"inventory entry %d sets %s for %d nodes; that field describes one machine",
						i+1, field, ns.Len())
				}
			}
			for _, name := range ns.Expand() {
				inv.apply(name, spec.Defaults, entry)
			}
		}
	}
	sort.Strings(inv.order)
	return inv, nil
}

// singleNodeField names the first field of an entry that only makes sense for
// one machine.
func singleNodeField(e v1alpha1.NodeEntry) string {
	switch {
	case e.Address != "":
		return "address"
	case e.BMCAddress != "":
		return "bmcAddress"
	case e.CID != "":
		return "cid"
	case len(e.MACs) > 0:
		return "macs"
	default:
		return ""
	}
}

func (inv *Inventory) apply(name string, defaults v1alpha1.NodeDefaults, e v1alpha1.NodeEntry) {
	node, ok := inv.nodes[name]
	if !ok {
		node = &Node{Name: name, Attributes: map[string]string{}}
		for k, v := range defaults.Attributes {
			node.Attributes[k] = v
		}
		inv.nodes[name] = node
		inv.order = append(inv.order, name)
	}
	for k, v := range e.Attributes {
		node.Attributes[k] = v
	}
	setIf(&node.Rack, e.Rack)
	setIf(&node.Level, e.Level)
	// The rack and the level are also exposed as attributes, so that a
	// group source reading an attribute can build one group per rack
	// without the rack having to be written twice.
	setAttr(node.Attributes, "rack", node.Rack)
	setAttr(node.Attributes, "level", node.Level)
	setIf(&node.Address, e.Address)
	setIf(&node.BMCAddress, e.BMCAddress)
	setIf(&node.CID, e.CID)
	setIf(&node.BootPath, e.BootPath)
	if len(e.MACs) > 0 {
		node.MACs = append([]string(nil), e.MACs...)
	}
}

func setIf(dst *string, value string) {
	if value != "" {
		*dst = value
	}
}

// setAttr mirrors a field into the attribute table, leaving an attribute the
// entry set explicitly alone.
func setAttr(attrs map[string]string, key, value string) {
	if value == "" {
		return
	}
	attrs[key] = value
}

// Len reports how many nodes the inventory knows.
func (inv *Inventory) Len() int { return len(inv.nodes) }

// Lookup returns a node by name.
func (inv *Inventory) Lookup(name string) (*Node, bool) {
	if n, ok := inv.nodes[name]; ok {
		return n, true
	}
	// The name may have been written with different padding.
	canonical, ok := inv.NodeSet().Canonical(name)
	if !ok {
		return nil, false
	}
	n, ok := inv.nodes[canonical]
	return n, ok
}

// Names returns every known node name in sorted order.
func (inv *Inventory) Names() []string {
	return append([]string(nil), inv.order...)
}

// All returns every node in name order.
func (inv *Inventory) All() []*Node {
	out := make([]*Node, 0, len(inv.order))
	for _, name := range inv.order {
		out = append(out, inv.nodes[name])
	}
	return out
}

// NodeSet returns every known node as a set.
func (inv *Inventory) NodeSet() *nodeset.NodeSet {
	ns := nodeset.New()
	for _, name := range inv.order {
		_ = ns.Add(name)
	}
	return ns
}

// Select returns the nodes of a set that the inventory knows, and the names
// of those it does not.
//
// A name is matched through the node set, so that a set written as exe[1-2]
// finds the nodes an inventory wrote as exe0001 and exe0002: padding is a
// display property, not part of a host's identity.
func (inv *Inventory) Select(ns *nodeset.NodeSet) (known []*Node, unknown []string) {
	all := inv.NodeSet()
	for _, name := range ns.Expand() {
		canonical, ok := all.Canonical(name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		known = append(known, inv.nodes[canonical])
	}
	return known, unknown
}

// Resolve returns the name the inventory uses for a host, which may differ
// from the name given only in padding.
func (inv *Inventory) Resolve(name string) (string, bool) {
	return inv.NodeSet().Canonical(name)
}

// AttributeValues lists the distinct values of an attribute, in sorted order.
func (inv *Inventory) AttributeValues(key string) []string {
	seen := map[string]bool{}
	for _, name := range inv.order {
		if v, ok := inv.nodes[name].Attributes[key]; ok && v != "" {
			seen[v] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// AttributeKeys lists every attribute name in the inventory.
func (inv *Inventory) AttributeKeys() []string {
	seen := map[string]bool{}
	for _, name := range inv.order {
		for k := range inv.nodes[name].Attributes {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WithAttribute returns the nodes whose attribute has the given value. An
// empty value matches every node that carries the attribute at all, the way
// a genders attribute without a value did.
func (inv *Inventory) WithAttribute(key, value string) *nodeset.NodeSet {
	ns := nodeset.New()
	for _, name := range inv.order {
		v, ok := inv.nodes[name].Attributes[key]
		if !ok {
			continue
		}
		if value == "" || v == value {
			_ = ns.Add(name)
		}
	}
	return ns
}

// Racks lists the racks the inventory knows, in sorted order.
func (inv *Inventory) Racks() []string {
	return inv.AttributeValuesOf(func(n *Node) string { return n.Rack })
}

// AttributeValuesOf lists the distinct non-empty values a field takes.
func (inv *Inventory) AttributeValuesOf(field func(*Node) string) []string {
	seen := map[string]bool{}
	for _, name := range inv.order {
		if v := field(inv.nodes[name]); v != "" {
			seen[v] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// InRack returns the nodes of one rack.
func (inv *Inventory) InRack(rack string) *nodeset.NodeSet {
	ns := nodeset.New()
	for _, name := range inv.order {
		if strings.EqualFold(inv.nodes[name].Rack, rack) {
			_ = ns.Add(name)
		}
	}
	return ns
}

// BootPath returns the boot path configured for a node: the one on its
// inventory entry, else the first cluster rule that names it.
//
// A node matched by more than one rule is an error rather than a silent
// first-match, because two rules pointing at different installations is a
// mistake worth stopping for.
func BootPath(inv *Inventory, rules []v1alpha1.BootPathRule, node string) (string, bool, error) {
	if n, ok := inv.Lookup(node); ok && n.BootPath != "" {
		return n.BootPath, false, nil
	}
	var (
		found  string
		static bool
		count  int
	)
	for i, rule := range rules {
		ns, err := nodeset.Parse(rule.Nodes)
		if err != nil {
			return "", false, fmt.Errorf("boot path rule %d: %w", i+1, err)
		}
		if ns.Contains(node) {
			count++
			found, static = rule.Path, rule.Static
		}
	}
	switch count {
	case 0:
		return "", false, fmt.Errorf("no boot path is configured for %s", node)
	case 1:
		return found, static, nil
	default:
		return "", false, fmt.Errorf("%d boot path rules name %s; exactly one must", count, node)
	}
}
