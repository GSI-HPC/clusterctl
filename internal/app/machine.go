// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
	"github.com/GSI-HPC/clusterctl/internal/naming"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// machines maps every name a site gives a machine to the name its inventory
// uses: the inventory name in any case, the host name and the service
// processor name the naming rules give it, and the addresses the inventory
// records.
type machines struct {
	// aliases maps a lowercased name or address to the inventory name. An
	// alias two machines share maps to "", so that it names neither.
	aliases map[string]string
	// known holds the lowercased inventory names, for finding a name that
	// was written with other padding.
	known *nodeset.NodeSet
	// names maps a lowercased inventory name back to the inventory's own.
	names map[string]string
}

// machineIndex builds the index of the inventory once.
func (a *App) machineIndex() *machines {
	a.machinesOnce.Do(func() {
		m := &machines{aliases: map[string]string{}, known: nodeset.New(), names: map[string]string{}}
		if a.Inventory != nil {
			nodes := a.Inventory.All()
			// An inventory name is never taken over by another machine's
			// alias, so the names go in first.
			for _, n := range nodes {
				lower := strings.ToLower(n.Name)
				m.names[lower] = n.Name
				m.aliases[lower] = n.Name
				_ = m.known.Add(lower)
			}
			for _, n := range nodes {
				var aliases []string
				if a.Namer != nil {
					if fqdn, err := a.Namer.FQDN(n.Name); err == nil {
						aliases = append(aliases, fqdn)
					}
					if bmc, err := a.Namer.BMC(n.Name); err == nil {
						aliases = append(aliases, bmc)
					}
				}
				aliases = append(aliases, n.Address, n.BMCAddress)
				for _, alias := range aliases {
					m.alias(alias, n.Name)
				}
			}
		}
		a.machines = m
	})
	return a.machines
}

func (m *machines) alias(alias, name string) {
	alias = strings.TrimSuffix(strings.ToLower(alias), ".")
	if alias == "" {
		return
	}
	if _, isName := m.names[alias]; isName {
		return
	}
	if other, ok := m.aliases[alias]; ok && other != name {
		m.aliases[alias] = ""
		return
	}
	m.aliases[alias] = name
}

// machine returns the name the inventory uses for the machine a name refers
// to.
//
// Host names are not case sensitive and a final dot only makes a name
// absolute, so WLM01 and wlm01. are wlm01. A host name or service processor
// name the naming rules give a node, and an address the inventory records
// for it, are that node too; so is its name written with other padding. A
// name with any other domain is left as it is, lowercased: it is not known
// to be the node, and the gate treats it with suspicion.
//
// The name the inventory writes is always its node's. Any other spelling of
// it that another machine's name or address is as well could be either
// machine, and is refused: exe4, the bmcAddress of exe0003, would otherwise
// reach exe0003 while EXE04 reaches exe0004.
func (a *App) machine(name string) (string, error) {
	return a.machineOf(name, name)
}

// machineOf is machine for a name that was selected as typed, which an
// error names.
func (a *App) machineOf(name, typed string) (string, error) {
	m := a.machineIndex()
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	if node, ok := m.names[n]; ok {
		return node, nil
	}
	spelled, err := a.spelling(n, typed)
	if err != nil {
		return "", err
	}
	node, ok := m.aliases[n]
	if !ok {
		return spelled, nil
	}
	if _, own := m.names[spelled]; own && node != spelled {
		other := "a name or address more than one machine has"
		if node != "" {
			other = "a name or address of " + node
		}
		return "", exitcode.Errorf(exitcode.Usage,
			"node %q is ambiguous: it is %s written another way, and %s; write the inventory name of the machine meant",
			typed, spelled, other)
	}
	if node == "" {
		// Two machines share this name; it names neither.
		return n, nil
	}
	return node, nil
}

// spelling returns the machine a lowercased name is another spelling of,
// with other padding, or as the host name or service processor name the
// naming rules give such a name, and otherwise the name itself.
func (a *App) spelling(n, typed string) (string, error) {
	m := a.machineIndex()
	if canonical, ok := m.known.Canonical(n); ok {
		return m.names[canonical], nil
	}
	if short := naming.Short(n); short != n && a.Namer != nil && !hostname.IsIP(n) {
		if fqdn, err := a.Namer.FQDN(short); err == nil && strings.EqualFold(fqdn, n) {
			return a.machineOf(short, typed)
		}
		if bmc, err := a.Namer.BMC(short); err == nil && strings.EqualFold(bmc, n) {
			return a.machineOf(short, typed)
		}
	}
	return n, nil
}

// protectedHosts resolves one safety.protectedHosts entry into machines,
// the way a selection is resolved, so that the gate compares machines rather
// than spellings. An entry naming a machine the inventory does not know is
// refused: it most likely protects nothing.
func (a *App) protectedHosts(expr string) (*nodeset.NodeSet, error) {
	ns, err := nodeset.ParseWith(expr, a.Groups)
	if err != nil {
		return nil, err
	}
	if ns, err = a.canonicalize(ns); err != nil {
		return nil, err
	}
	if a.Inventory != nil && a.Inventory.Len() > 0 {
		if unknown := ns.Difference(a.Inventory.NodeSet()); !unknown.IsEmpty() {
			return nil, exitcode.Errorf(exitcode.Usage,
				"it names %s, which the inventory does not know; write the inventory name of the machine", unknown)
		}
	}
	return ns, nil
}
