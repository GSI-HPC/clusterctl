// SPDX-License-Identifier: LGPL-3.0-or-later

// Package naming turns short node names into host names and service
// processor names, replacing the prefix logic the shell tools carried.
package naming

import (
	"fmt"
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
		compiled := rule{prefixes: r.Match.Prefixes, fqdn: r.FQDN, bmc: r.BMC}
		if r.Match.Pattern != "" {
			re, err := regexp.Compile(r.Match.Pattern)
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
// administrator can always name a host exactly.
func (n *Namer) FQDN(node string) (string, error) {
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

// BMC returns the host name of a node's service processor. Any domain on the
// input is dropped first, because the service processor lives in a different
// domain than the node.
func (n *Namer) BMC(node string) (string, error) {
	short := node
	if i := strings.IndexByte(short, '.'); i >= 0 {
		short = short[:i]
	}
	r, ok := n.match(short)
	if !ok || r.bmc == "" {
		return short, nil
	}
	return n.expand(r.bmc, short)
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
	for k, v := range n.vars {
		vars[k] = v
	}
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

// BMCSet maps every node of a set to its service processor name.
func (n *Namer) BMCSet(ns *nodeset.NodeSet) (*nodeset.NodeSet, error) {
	return n.mapSet(ns, n.BMC)
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
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}
