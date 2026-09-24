// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"regexp"
	"strings"
	"testing"
)

// inventoryPositions finds the file:line references to the example
// inventory in an error.
var inventoryPositions = regexp.MustCompile(`inventory\.yaml:\d+`)

// Report 4.3: exe1 written after exe[0001-0010] made a thirteenth node that no
// selection reached, so its class, its address and everything else it said
// were silently ignored. It names the same host as exe0001, and which of the
// two the author meant cannot be told, so the inventory is refused.
func TestInventoryRefusesTwoSpellingsOfOneHost(t *testing.T) {
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe1\n      attributes: {class: spare}\n" +
			"    - nodes: exe2\n      address: 10.0.2.2\n"
	})
	for _, args := range [][]string{{"config", "validate"}, {"node", "list"}} {
		_, err := run(t, harnessOptions{config: []string{inventory}}, args...)
		if err == nil {
			t.Fatalf("%s accepted an inventory naming exe0001 twice", strings.Join(args, " "))
		}
		msg := err.Error()
		if !strings.Contains(msg, "exe1") || !strings.Contains(msg, "exe0001") {
			t.Errorf("error = %v, want it to name both spellings", err)
		}
		if got := inventoryPositions.FindAllString(msg, -1); len(got) != 2 || got[0] == got[1] {
			t.Errorf("error = %v, want the file and line of both entries", err)
		}
	}
}

// Report 4.6: one unpadded name next to a padded range made node list
// dereference a nil node.
func TestNodeListWithMixedWidths(t *testing.T) {
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe11\n      attributes: {class: exe}\n      rack: R02\n"
	})
	for args, want := range map[string]string{
		"exe11": "1 nodes",
		"@exe":  "11 nodes",
	} {
		h, err := run(t, harnessOptions{config: []string{inventory}}, "node", "list", args)
		if err != nil {
			t.Fatalf("node list %s failed: %v", args, err)
		}
		if out := h.out.String(); !strings.Contains(out, want) || !strings.Contains(out, "exe11") {
			t.Errorf("node list %s:\n%s\nwant exe11 and %q", args, out, want)
		}
	}
}
