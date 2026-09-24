// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package hostname decides whether a name may be handed to ssh or put into
// a URL as a host.
//
// A node name reaches places that give some characters a meaning: ssh reads
// a leading "-" as an option, and a URL reads ":", "@", "/", "?" and "#" as
// the port, the userinfo, the path, the query and the fragment. Checking each
// place for the characters it happens to care about would miss the next one,
// so a name is accepted only when it is spelled in the host name alphabet of
// RFC 1123, in which none of them has a meaning.
package hostname

import (
	"fmt"
	"net"
	"strings"
)

const (
	// maxName is the longest host name DNS can carry, without the final dot.
	maxName = 253
	// maxLabel is the longest label between two dots.
	maxLabel = 63
)

// Check reports whether name is a host name: dot separated labels of ASCII
// letters, digits and hyphens, none of them empty, none longer than 63
// characters, and none beginning or ending with a hyphen. A single final dot
// is allowed, since it only says the name is fully qualified.
//
// An IPv4 address in dotted form passes, because it is spelled in the same
// alphabet. An IPv6 address does not; use CheckHost where one is expected.
func Check(name string) error {
	if name == "" {
		return fmt.Errorf("the host name is empty")
	}
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" {
		return fmt.Errorf("%q is not a host name", name)
	}
	if len(trimmed) > maxName {
		return fmt.Errorf("%q is not a host name: it is longer than %d characters", name, maxName)
	}
	for _, label := range strings.Split(trimmed, ".") {
		if err := checkLabel(label); err != nil {
			return fmt.Errorf("%q is not a host name: %w", name, err)
		}
	}
	return nil
}

func checkLabel(label string) error {
	switch {
	case label == "":
		return fmt.Errorf("it has an empty label")
	case len(label) > maxLabel:
		return fmt.Errorf("the label %q is longer than %d characters", label, maxLabel)
	case label[0] == '-':
		return fmt.Errorf("the label %q begins with a hyphen", label)
	case label[len(label)-1] == '-':
		return fmt.Errorf("the label %q ends with a hyphen", label)
	}
	for i := 0; i < len(label); i++ {
		if !isHostChar(label[i]) {
			return fmt.Errorf("%q is not a letter, a digit or a hyphen", rune(label[i]))
		}
	}
	return nil
}

func isHostChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// CheckHost reports whether host names a machine on its own: a host name as
// Check accepts it, or an IPv4 or IPv6 address. Anything that would add a
// port, an account, a zone or a path to the host is refused.
func CheckHost(host string) error {
	if IsIP(host) {
		return nil
	}
	return Check(host)
}

// IsIP reports whether host is an IPv4 or IPv6 address without a zone, and
// without the brackets a URL puts around an IPv6 address.
func IsIP(host string) bool {
	return !strings.Contains(host, "%") && net.ParseIP(host) != nil
}
