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
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
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
	// all holds every node name. It is built once, because finding the
	// node a name written with other padding refers to needs it for every
	// name of every selection.
	all *nodeset.NodeSet
}

// Document is one NodeInventory together with a way to say where each of its
// entries was written, so that an error can point at the lines to fix.
type Document struct {
	Spec v1alpha1.NodeInventorySpec
	// Where returns where entry i, counted from zero, was written, such as
	// "inventory.yaml:31:7". It may be nil, or return "" when that is not
	// known.
	Where func(entry int) string
}

// New builds an inventory from the NodeInventory documents of a site.
//
// Entries are applied in order, so a general entry may be written first and
// refined by a later one naming fewer nodes. Fields that describe a single
// machine may only be set by an entry that names exactly one node.
func New(specs ...v1alpha1.NodeInventorySpec) (*Inventory, error) {
	docs := make([]Document, len(specs))
	for i, spec := range specs {
		docs[i] = Document{Spec: spec}
	}
	return FromDocuments(docs...)
}

// FromDocuments is New for documents that know where their entries were
// written.
//
// Addresses, service processor addresses, cids and MACs identify one machine,
// so two nodes sharing one are refused: a copied entry would otherwise send
// an action meant for one machine to another. An address must be an IP
// address and a MAC a MAC address, because both end up in the names of files
// on the PXE server and in DHCP.
//
// A host is named the same way in every entry. Padding and case are not part
// of a host's identity, so exe1, exe0001 and EXE1 are one machine; an entry
// spelling a host differently from the entry that first named it would either
// make a second record for the same machine or silently merge into the first,
// and which one the author meant cannot be told, so it is refused.
func FromDocuments(docs ...Document) (*Inventory, error) {
	inv := &Inventory{nodes: map[string]*Node{}}
	b := &builder{
		inv:      inv,
		folded:   nodeset.New(),
		spelling: map[string]string{},
		named:    map[string]string{},
		setBy:    map[string]map[string]string{},
	}
	for d, doc := range docs {
		for i, entry := range doc.Spec.Nodes {
			label := entryLabel(docs, d, i)
			ns, err := nodeset.Parse(entry.Nodes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
			if ns.IsEmpty() {
				return nil, fmt.Errorf("%s names no node", label)
			}
			if ns.Len() > 1 {
				if field := singleNodeField(entry); field != "" {
					return nil, fmt.Errorf(
						"%s sets %s for %d nodes; that field describes one machine",
						label, field, ns.Len())
				}
			}
			if err := checkIdentifiers(entry); err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
			for _, name := range ns.Expand() {
				if err := b.claim(name, label); err != nil {
					return nil, err
				}
				inv.apply(name, doc.Spec.Defaults, entry)
				b.record(name, label, entry)
			}
		}
	}
	if err := b.checkUnique(); err != nil {
		return nil, err
	}
	sort.Strings(inv.order)
	inv.all = nodeset.New()
	for _, name := range inv.order {
		if err := inv.all.Add(name); err != nil {
			return nil, err
		}
	}
	return inv, nil
}

// entryLabel names an entry the way an error shows it.
func entryLabel(docs []Document, d, i int) string {
	label := fmt.Sprintf("inventory entry %d", i+1)
	if len(docs) > 1 {
		label = fmt.Sprintf("inventory %d entry %d", d+1, i+1)
	}
	if where := docs[d].Where; where != nil {
		if w := where(i); w != "" {
			label += " (" + w + ")"
		}
	}
	return label
}

// builder holds what building an inventory needs to know besides the nodes.
type builder struct {
	inv *Inventory
	// folded holds every name given so far, lowercased, so that a name
	// written with other padding or case finds the host it refers to.
	folded *nodeset.NodeSet
	// spelling maps a name of folded back to the name it was given as.
	spelling map[string]string
	// named records the entry that first named each host.
	named map[string]string
	// setBy records, for each host and field, the entry that last set it.
	setBy map[string]map[string]string
}

// record notes which fields identifying a machine an entry set for a host.
func (b *builder) record(name, label string, e v1alpha1.NodeEntry) {
	fields := b.setBy[name]
	if fields == nil {
		fields = map[string]string{}
		b.setBy[name] = fields
	}
	for field, set := range map[string]bool{
		"address": e.Address != "", "bmcAddress": e.BMCAddress != "", "cid": e.CID != "", "macs": len(e.MACs) > 0,
	} {
		if set {
			fields[field] = label
		}
	}
}

// identifier is a value that belongs to one machine, and the node and field
// it was given in.
type identifier struct {
	node, field, value string
}

// checkUnique refuses two nodes sharing an identifier, judged on what the
// inventory ends up holding, so that a refinement moving an address away
// frees it. An address and a service processor address share one space: a
// node's address given to another node's service processor is as wrong.
func (b *builder) checkUnique() error {
	seen := map[string]identifier{}
	for _, name := range b.inv.order {
		n := b.inv.nodes[name]
		var ids []identifier
		if n.Address != "" {
			ids = append(ids, identifier{name, "address", n.Address})
		}
		if n.BMCAddress != "" {
			ids = append(ids, identifier{name, "bmcAddress", n.BMCAddress})
		}
		if n.CID != "" {
			ids = append(ids, identifier{name, "cid", n.CID})
		}
		for _, mac := range n.MACs {
			ids = append(ids, identifier{name, "macs", mac})
		}
		for _, id := range ids {
			key := identifierKey(id)
			first, ok := seen[key]
			if !ok {
				seen[key] = id
				continue
			}
			if first.node == name {
				continue
			}
			return fmt.Errorf("%s gives %s %s %s, which is %s %s of %s, given by %s; "+
				"each machine needs its own",
				b.setBy[name][id.field], name, fieldNames[id.field], id.value,
				fieldNames[first.field], first.value, first.node, b.setBy[first.node][first.field])
		}
	}
	return nil
}

// fieldNames names the fields identifying a machine the way an error does.
var fieldNames = map[string]string{
	"address": "address", "bmcAddress": "bmcAddress", "cid": "cid", "macs": "MAC",
}

// identifierKey is what two identifiers must share to name one machine,
// whatever way each was written.
func identifierKey(id identifier) string {
	switch id.field {
	case "cid":
		return "cid\x00" + id.value
	case "macs":
		if hw, err := net.ParseMAC(id.value); err == nil {
			return "mac\x00" + hw.String()
		}
	default:
		if addr, err := ipAddress(id.value); err == nil {
			return "address\x00" + addr.String()
		}
	}
	return "address\x00" + strings.TrimSuffix(strings.ToLower(id.value), ".")
}

// checkIdentifiers refuses an entry whose identifiers are not what their
// field holds.
func checkIdentifiers(e v1alpha1.NodeEntry) error {
	if e.Address != "" {
		if _, err := ipAddress(e.Address); err != nil {
			return fmt.Errorf("address %q is not an IP address", e.Address)
		}
	}
	if e.BMCAddress != "" {
		if _, err := ipAddress(e.BMCAddress); err != nil && hostname.Check(e.BMCAddress) != nil {
			return fmt.Errorf("bmcAddress %q is neither an IP address nor a host name", e.BMCAddress)
		}
	}
	for _, mac := range e.MACs {
		if hw, err := net.ParseMAC(mac); err != nil || len(hw) != 6 {
			return fmt.Errorf("%q in macs is not a 48-bit MAC address", mac)
		}
	}
	return nil
}

// ipAddress parses an IP address, refusing a prefix length or a zone, which
// no machine's address has and which would end up in a file name.
func ipAddress(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	if addr.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%q has a zone", s)
	}
	return addr.Unmap(), nil
}

// claim records that an entry names a host, and refuses a name that spells a
// host already named differently.
func (b *builder) claim(name, label string) error {
	lower := strings.ToLower(name)
	if held, ok := b.folded.Canonical(lower); ok {
		if first := b.spelling[held]; first != name {
			return fmt.Errorf("%s names %s, which %s wrote as %s; "+
				"names differing only in padding or case are one host, so write it %s in both",
				label, name, b.named[first], first, first)
		}
		return nil
	}
	if err := b.folded.Add(lower); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	b.spelling[lower] = name
	b.named[name] = label
	return nil
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
	canonical, ok := inv.all.Canonical(name)
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
	return inv.all.Clone()
}

// Select returns the nodes of a set that the inventory knows, and the names
// of those it does not.
//
// A name is matched through the node set, so that a set written as exe[1-2]
// finds the nodes an inventory wrote as exe0001 and exe0002: padding is a
// display property, not part of a host's identity.
func (inv *Inventory) Select(ns *nodeset.NodeSet) (known []*Node, unknown []string) {
	for _, name := range ns.Expand() {
		canonical, ok := inv.all.Canonical(name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		// Every name of the set is a key, but a miss is still reported as
		// unknown rather than returned as a nil node.
		n, ok := inv.nodes[canonical]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		known = append(known, n)
	}
	return known, unknown
}

// Resolve returns the name the inventory uses for a host, which may differ
// from the name given only in padding.
func (inv *Inventory) Resolve(name string) (string, bool) {
	return inv.all.Canonical(name)
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
