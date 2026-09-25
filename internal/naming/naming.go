// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package naming turns short node names into host names and service
// processor names, replacing the prefix logic the shell tools carried.
package naming

import (
	"fmt"
	"maps"
	"net"
	"regexp"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/tmpl"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Namer applies the naming rules of a site.
type Namer struct {
	rules   []rule
	vars    map[string]string
	domains map[string]string
}

type rule struct {
	index    int
	prefixes []string
	pattern  *regexp.Regexp
	fqdn     string
	bmc      string
}

// New compiles the naming rules of a site.
func New(spec v1alpha1.NamingSpec, domains map[string]string) (*Namer, error) {
	n := &Namer{
		vars:    map[string]string{"bmcPrefix": spec.BMCPrefix},
		domains: domains,
	}
	tmpl.Prefixed("domains", domains, n.vars)

	for i, r := range spec.Rules {
		compiled := rule{index: i + 1, fqdn: r.FQDN, bmc: r.BMC}
		// Names are lowercased before they are matched, so are prefixes.
		for _, p := range r.Match.Prefixes {
			compiled.prefixes = append(compiled.prefixes, strings.ToLower(p))
		}
		if r.Match.Pattern != "" {
			// The pattern names the whole short name. Matched as a
			// substring, gpu[0-9]+ would also claim login-gpu01.
			re, err := regexp.Compile("^(?:" + r.Match.Pattern + ")$")
			if err != nil {
				return nil, fmt.Errorf("naming rule %d: invalid pattern %q: %w", i+1, r.Match.Pattern, err)
			}
			compiled.pattern = re
		}
		if compiled.fqdn == "" && compiled.bmc == "" {
			return nil, fmt.Errorf("naming rule %d sets neither fqdn nor bmc", i+1)
		}
		n.rules = append(n.rules, compiled)
	}
	if len(n.rules) == 0 {
		return nil, fmt.Errorf("the site defines no naming rules")
	}
	return n, nil
}

// Domain returns a named domain of the site.
func (n *Namer) Domain(role string) string { return n.domains[role] }

// FQDN returns the host name of a node.
//
// A name that already carries a domain is returned unchanged, so that an
// administrator can always name a host exactly. Host names are not case
// sensitive, so a name is lowercased first: WLM01 is wlm01, and gets its
// rule.
func (n *Namer) FQDN(node string) (string, error) {
	node = strings.ToLower(node)
	if strings.Contains(node, ".") {
		return node, nil
	}
	r, ok := n.match(node)
	if !ok {
		return node, nil
	}
	if r.fqdn == "" {
		return node, nil
	}
	return n.expand(r.fqdn, node)
}

// BMC returns the host name of a node's service processor.
//
// The name comes from the bmc template of the first rule that matches the
// short name, and nothing else: a node whose rule has none, or that no rule
// matches, is refused rather than given its own name, because the site's BMC
// account would then be sent to the node. For the same reason a template
// that gives the node's short name or host name back is refused. A node
// whose service processor cannot be named by a rule has its address written
// in the inventory, which the commands prefer.
//
// A name with a domain is accepted only when it is the host name the rules
// give its short name. Any other domain names a different machine, whose
// service processor the rules do not know. An IP address is refused too: it
// has no short name, and cutting it at the first dot named one host for a
// whole subnet.
func (n *Namer) BMC(node string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(node), ".")
	if net.ParseIP(name) != nil {
		return "", fmt.Errorf("%q is an address, not a node name; the naming rules cannot name its service processor", node)
	}
	short := Short(name)
	fqdn, err := n.FQDN(short)
	if err != nil {
		return "", err
	}
	if name != short && !strings.EqualFold(name, fqdn) {
		return "", fmt.Errorf("%q is not the host name the naming rules give %s, which is %s; "+
			"name the node by its short name or its host name", node, short, fqdn)
	}
	r, ok := n.match(short)
	if !ok {
		return "", fmt.Errorf("no naming rule matches %s, so its service processor has no name; "+
			"add a rule with a bmc template or set bmcAddress in the inventory", short)
	}
	if r.bmc == "" {
		return "", fmt.Errorf("naming rule %d, which matches %s, has no bmc template, so its service processor "+
			"has no name; add one or set bmcAddress in the inventory", r.index, short)
	}
	bmc, err := n.expand(r.bmc, short)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(bmc, short) || strings.EqualFold(bmc, fqdn) {
		return "", fmt.Errorf("naming rule %d names the service processor of %s %s, which is the node itself; "+
			"give the rule a bmc template in another domain or set bmcAddress in the inventory", r.index, short, bmc)
	}
	return bmc, nil
}

func (n *Namer) match(node string) (rule, bool) {
	for _, r := range n.rules {
		switch {
		case r.pattern != nil:
			if r.pattern.MatchString(node) {
				return r, true
			}
		case len(r.prefixes) > 0:
			for _, p := range r.prefixes {
				if strings.HasPrefix(node, p) {
					return r, true
				}
			}
		default:
			// A rule with no match block matches every name.
			return r, true
		}
	}
	return rule{}, false
}

func (n *Namer) expand(template, node string) (string, error) {
	vars := make(map[string]string, len(n.vars)+1)
	maps.Copy(vars, n.vars)
	vars["name"] = node
	out, err := tmpl.Expand(template, vars)
	if err != nil {
		return "", err
	}
	// A rule whose domain is not configured would produce "exe1." and a
	// lookup that fails much later.
	if strings.HasSuffix(out, ".") || strings.Contains(out, "..") {
		return "", fmt.Errorf("naming %q with %q produced %q; a domain it refers to is not set", node, template, out)
	}
	return out, nil
}

// FQDNSet maps every node of a set to its host name.
func (n *Namer) FQDNSet(ns *nodeset.NodeSet) (*nodeset.NodeSet, error) {
	return n.mapSet(ns, n.FQDN)
}

func (n *Namer) mapSet(ns *nodeset.NodeSet, f func(string) (string, error)) (*nodeset.NodeSet, error) {
	out := nodeset.New()
	for _, node := range ns.Expand() {
		name, err := f(node)
		if err != nil {
			return nil, err
		}
		if err := out.Add(name); err != nil {
			return nil, fmt.Errorf("the name %q of node %q is not a valid host name: %w", name, node, err)
		}
	}
	return out, nil
}

// Short returns the node name without its domain.
func Short(name string) string {
	before, _, _ := strings.Cut(name, ".")
	return before
}
