// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// checkSiteHosts refuses a selection with a name that is not a host of the
// site: a node the inventory does not know, whose host name the naming rules
// do not put in one of the site's domains.
//
// A read command still connects to what it is given. At a terminal the
// administrator may name any host; an agent is held to the site, so that it
// cannot point a host key scan at a machine of its choosing or have a DNS
// lookup carry data out in a name.
func (a *App) checkSiteHosts(ns *nodeset.NodeSet) error {
	domains := make([]string, 0, len(a.Spec.Domains))
	for _, d := range a.Spec.Domains {
		if d = strings.Trim(strings.ToLower(d), "."); d != "" {
			domains = append(domains, d)
		}
	}
	sort.Strings(domains)
	for _, name := range ns.Expand() {
		if a.Inventory != nil {
			if _, ok := a.Inventory.Lookup(name); ok {
				continue
			}
		}
		host, err := a.Namer.FQDN(name)
		if err != nil {
			return exitcode.Wrap(exitcode.Usage, err)
		}
		if !inDomains(strings.TrimSuffix(host, "."), domains) {
			within := "the site names no domains"
			if len(domains) > 0 {
				within = "its host name " + host + " is in none of the site's domains (" + strings.Join(domains, ", ") + ")"
			}
			return exitcode.Errorf(exitcode.Usage,
				"node %q is not a host of the site: the inventory does not know it, and %s", name, within)
		}
	}
	return nil
}

// inDomains reports whether a host name lies below one of the domains.
func inDomains(host string, domains []string) bool {
	for _, d := range domains {
		if strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
