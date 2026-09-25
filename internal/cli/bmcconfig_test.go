// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// bmc.ipmi.passwordTransport, which nothing read, is no setting any more,
// and bmc.order accepted any word, which then meant Redfish.
func TestBMCSettingsTheCodeDoesNotKeepAreRefused(t *testing.T) {
	for _, edit := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, "driver: LAN_2_0", "driver: LAN_2_0\n      passwordTransport: file", 1)
		},
		func(s string) string { return strings.Replace(s, "order: [redfish, ipmi]", "order: [impi]", 1) },
	} {
		site := exampleWith(t, "site.yaml", edit)
		if _, err := run(t, harnessOptions{config: []string{site}}, "config", "validate"); err == nil {
			t.Errorf("config validate accepted %s", readFile(t, filepath.Join(site, "site.yaml")))
		}
	}
}
