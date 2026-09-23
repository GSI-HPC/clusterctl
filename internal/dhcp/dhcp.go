// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package dhcp reads an ISC dhcpd configuration.
//
// The host declarations are parsed rather than grepped. Reading a fixed
// number of lines after a match, which the shell tools did, breaks as soon as
// a declaration is one line longer or the options are in another order, and
// it silently reports a neighbour's address.
package dhcp

import (
	"fmt"
	"sort"
	"strings"
)

// Host is one host declaration.
type Host struct {
	// Name is the name the declaration carries.
	Name string `json:"name" yaml:"name"`
	// MACs are the hardware addresses it matches.
	MACs []string `json:"macs,omitempty" yaml:"macs,omitempty"`
	// Address is the fixed address it hands out.
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// Filename is the boot file it offers.
	Filename string `json:"filename,omitempty" yaml:"filename,omitempty"`
	// ClientIdentifier is the dhcp-client-identifier option it matches.
	ClientIdentifier string `json:"clientIdentifier,omitempty" yaml:"clientIdentifier,omitempty"`
	// Options holds every other option of the declaration.
	Options map[string]string `json:"options,omitempty" yaml:"options,omitempty"`
	// Comments are the comment lines directly above the declaration, which
	// is where sites often write the node name.
	Comments []string `json:"comments,omitempty" yaml:"comments,omitempty"`
}

// Config is a parsed dhcpd configuration.
type Config struct {
	Hosts []Host `json:"hosts" yaml:"hosts"`
}

// Parse reads a dhcpd configuration.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	var (
		pending []string
		current *Host
		depth   int
	)

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			if current == nil {
				pending = nil
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			if current == nil {
				pending = append(pending, strings.TrimSpace(strings.TrimPrefix(line, "#")))
			}
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}

		if current == nil {
			name, ok := hostDeclaration(line)
			if !ok {
				if strings.Contains(line, "{") {
					depth += strings.Count(line, "{") - strings.Count(line, "}")
					if depth < 0 {
						depth = 0
					}
				}
				pending = nil
				continue
			}
			current = &Host{Name: name, Options: map[string]string{}, Comments: pending}
			pending = nil
			// A declaration written on one line closes here.
			if strings.Contains(line, "}") {
				cfg.Hosts = append(cfg.Hosts, *current)
				current = nil
			}
			continue
		}

		if strings.HasPrefix(line, "}") {
			cfg.Hosts = append(cfg.Hosts, *current)
			current = nil
			continue
		}
		applyStatement(current, line)
	}
	if current != nil {
		return nil, fmt.Errorf("the host declaration %q is not closed", current.Name)
	}
	sort.Slice(cfg.Hosts, func(i, j int) bool { return cfg.Hosts[i].Name < cfg.Hosts[j].Name })
	return cfg, nil
}

// hostDeclaration recognises "host NAME {".
func hostDeclaration(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "host" {
		return "", false
	}
	name := strings.TrimSuffix(fields[1], "{")
	if name == "" {
		return "", false
	}
	return name, true
}

// applyStatement reads one statement of a host declaration.
func applyStatement(h *Host, line string) {
	statement := strings.TrimSuffix(strings.TrimSpace(line), ";")
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return
	}

	switch {
	case len(fields) >= 3 && fields[0] == "hardware" && fields[1] == "ethernet":
		h.MACs = append(h.MACs, strings.ToLower(fields[2]))
	case len(fields) >= 2 && fields[0] == "fixed-address":
		h.Address = strings.TrimSuffix(fields[1], ",")
	case len(fields) >= 2 && fields[0] == "filename":
		h.Filename = unquote(strings.Join(fields[1:], " "))
	case len(fields) >= 3 && fields[0] == "option" && fields[1] == "dhcp-client-identifier":
		h.ClientIdentifier = unquote(strings.Join(fields[2:], " "))
	case len(fields) >= 3 && fields[0] == "option":
		h.Options[fields[1]] = unquote(strings.Join(fields[2:], " "))
	default:
		h.Options[fields[0]] = unquote(strings.Join(fields[1:], " "))
	}
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// Lookup finds the declarations belonging to a node.
//
// A declaration matches when its name is the node name, when its name starts
// with the node name followed by a separator, which is how a second interface
// is usually written, or when a comment above it names the node.
func (c *Config) Lookup(node string) []Host {
	var out []Host
	for _, h := range c.Hosts {
		if matchesNode(h, node) {
			out = append(out, h)
		}
	}
	return out
}

func matchesNode(h Host, node string) bool {
	name := h.Name
	if name == node {
		return true
	}
	if strings.HasPrefix(name, node) {
		rest := name[len(node):]
		if rest != "" && (rest[0] == '-' || rest[0] == '.' || rest[0] == '_') {
			return true
		}
	}
	// The short name of a declaration written as a fully qualified name.
	if short, _, ok := strings.Cut(name, "."); ok && short == node {
		return true
	}
	for _, comment := range h.Comments {
		for _, word := range strings.Fields(comment) {
			if word == node {
				return true
			}
		}
	}
	return false
}

// GUIDFromMAC derives the InfiniBand port GUID of a Mellanox adapter from its
// hardware address, which is what the fabric tools are asked about.
//
// The address is split in the middle and 0300 is inserted, which is the
// mapping Mellanox firmware uses between the two.
func GUIDFromMAC(mac string) (string, error) {
	clean := strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.TrimSpace(mac))
	if len(clean) != 12 {
		return "", fmt.Errorf("%q is not a hardware address", mac)
	}
	for _, c := range clean {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "", fmt.Errorf("%q is not a hardware address", mac)
		}
	}
	clean = strings.ToLower(clean)
	return "0x" + clean[:6] + "0300" + clean[6:], nil
}
