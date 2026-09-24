// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/dhcp"
)

// FuzzParse checks that parsing never panics, and that whatever it accepts
// can be asked for a node's boot address.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		sample,
		"host a { fixed-address 10.0.0.1; }",
		"host a {\n  fixed-address 10.0.0.1; }\nhost b {\n}\n",
		"group { host a { hardware ethernet aa:bb:cc:dd:ee:ff; } }",
		"option foo code 224 = { unsigned integer 8, text };",
		"host \"a\" { filename \"x#y\"; }",
		"}", "{", "host", "host a {", "\"", "# only a comment",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, conf string) {
		if len(conf) > 1<<12 {
			return
		}
		cfg, err := dhcp.Parse([]byte(conf))
		if err != nil {
			return
		}
		for _, h := range cfg.Hosts {
			if h.Name == "" {
				t.Fatalf("a declaration without a name was accepted: %q", conf)
			}
			address, err := cfg.BootAddress(h.Name)
			if err == nil && address == "" {
				t.Fatalf("BootAddress(%q) returned no address and no error", h.Name)
			}
			cfg.Lookup(h.Name)
			cfg.Mentions(h.Name)
		}
	})
}
