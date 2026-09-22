// SPDX-License-Identifier: LGPL-3.0-or-later

package inventory_test

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func exampleSpec() v1alpha1.NodeInventorySpec {
	return v1alpha1.NodeInventorySpec{
		Defaults: v1alpha1.NodeDefaults{Attributes: map[string]string{"os": "el9"}},
		Nodes: []v1alpha1.NodeEntry{
			{
				Nodes:      "exe[1-4]",
				Attributes: map[string]string{"class": "exe", "vendor": "vendor2"},
				Rack:       "R02",
			},
			{
				Nodes:      "wlm01",
				Attributes: map[string]string{"class": "wlm", "vendor": "vendor1"},
				Rack:       "R01",
				Address:    "10.0.1.1",
				CID:        "123",
			},
			// A later entry refines the nodes it names.
			{Nodes: "exe1", Address: "10.0.2.1", MACs: []string{"00:11:22:33:44:55"}, Level: "1"},
		},
	}
}

func exampleInventory(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.New(exampleSpec())
	if err != nil {
		t.Fatalf("building the inventory: %v", err)
	}
	return inv
}

func TestNewAppliesDefaultsAndRefinements(t *testing.T) {
	t.Parallel()
	inv := exampleInventory(t)

	if got, want := inv.Len(), 5; got != want {
		t.Errorf("Len = %d, want %d", got, want)
	}

	exe1, ok := inv.Lookup("exe1")
	if !ok {
		t.Fatal("exe1 is missing from the inventory")
	}
	if got, want := exe1.Attributes["os"], "el9"; got != want {
		t.Errorf("the default attribute was not applied: os = %q, want %q", got, want)
	}
	if got, want := exe1.Attributes["class"], "exe"; got != want {
		t.Errorf("class = %q, want %q", got, want)
	}
	if got, want := exe1.Address, "10.0.2.1"; got != want {
		t.Errorf("the later entry did not refine the address: %q, want %q", got, want)
	}
	if got, want := exe1.Rack, "R02"; got != want {
		t.Errorf("the refinement dropped the rack: %q, want %q", got, want)
	}

	exe2, _ := inv.Lookup("exe2")
	if exe2.Address != "" {
		t.Errorf("the refinement leaked onto exe2: address = %q", exe2.Address)
	}
}

func TestSingleNodeFieldsAreRejectedForSets(t *testing.T) {
	t.Parallel()

	spec := v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[1-4]", Address: "10.0.1.1"},
	}}
	_, err := inventory.New(spec)
	if err == nil {
		t.Fatal("setting one address for four nodes should be rejected")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

func TestNewRejectsBadEntries(t *testing.T) {
	t.Parallel()

	cases := map[string]v1alpha1.NodeInventorySpec{
		"a malformed node set": {Nodes: []v1alpha1.NodeEntry{{Nodes: "exe["}}},
		"an empty node set":    {Nodes: []v1alpha1.NodeEntry{{Nodes: ""}}},
	}
	for name, spec := range cases {
		if _, err := inventory.New(spec); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestQueries(t *testing.T) {
	t.Parallel()
	inv := exampleInventory(t)

	if got, want := inv.WithAttribute("class", "exe").String(), "exe[1-4]"; got != want {
		t.Errorf("WithAttribute = %q, want %q", got, want)
	}
	// An empty value matches every node carrying the attribute, the way a
	// valueless genders attribute did.
	if got, want := inv.WithAttribute("os", "").Len(), 5; got != want {
		t.Errorf("WithAttribute with no value matched %d nodes, want %d", got, want)
	}
	if got, want := strings.Join(inv.AttributeValues("class"), ","), "exe,wlm"; got != want {
		t.Errorf("AttributeValues = %q, want %q", got, want)
	}
	// The rack is mirrored into the attributes, so a group source reading
	// an attribute can build one group per rack.
	if got, want := strings.Join(inv.AttributeKeys(), ","), "class,level,os,rack,vendor"; got != want {
		t.Errorf("AttributeKeys = %q, want %q", got, want)
	}
	if got, want := inv.WithAttribute("rack", "R02").String(), "exe[1-4]"; got != want {
		t.Errorf("grouping by the rack attribute = %q, want %q", got, want)
	}
	if got, want := strings.Join(inv.Racks(), ","), "R01,R02"; got != want {
		t.Errorf("Racks = %q, want %q", got, want)
	}
	if got, want := inv.InRack("r02").String(), "exe[1-4]"; got != want {
		t.Errorf("InRack = %q, want %q", got, want)
	}
	if got, want := inv.NodeSet().String(), "exe[1-4],wlm01"; got != want {
		t.Errorf("NodeSet = %q, want %q", got, want)
	}
	if got, want := len(inv.All()), 5; got != want {
		t.Errorf("All returned %d nodes, want %d", got, want)
	}
}

func TestSelectReportsUnknownNodes(t *testing.T) {
	t.Parallel()
	inv := exampleInventory(t)

	known, unknown := inv.Select(nodeset.MustParse("exe[1-2],ghost1"))
	if got, want := len(known), 2; got != want {
		t.Errorf("known = %d, want %d", got, want)
	}
	if got, want := strings.Join(unknown, ","), "ghost1"; got != want {
		t.Errorf("unknown = %q, want %q", got, want)
	}
}

func TestBootPath(t *testing.T) {
	t.Parallel()

	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[1-4]"},
		{Nodes: "exe4", BootPath: "/srv/pxesrv/boot/special"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rules := []v1alpha1.BootPathRule{
		{Nodes: "exe[1-2]", Path: "/srv/pxesrv/boot/exe"},
		{Nodes: "exe3", Path: "/srv/pxesrv/boot/exe", Static: true},
		{Nodes: "exe3", Path: "/srv/pxesrv/boot/other"},
	}

	path, static, err := inventory.BootPath(inv, rules, "exe1")
	if err != nil || path != "/srv/pxesrv/boot/exe" || static {
		t.Errorf("BootPath(exe1) = %q, %v, %v", path, static, err)
	}
	// The node's own entry wins over the cluster rules.
	path, _, err = inventory.BootPath(inv, rules, "exe4")
	if err != nil || path != "/srv/pxesrv/boot/special" {
		t.Errorf("BootPath(exe4) = %q, %v", path, err)
	}
	// Two rules naming one node is a mistake worth stopping for.
	if _, _, err := inventory.BootPath(inv, rules, "exe3"); err == nil {
		t.Error("two rules for one node should be reported")
	}
	if _, _, err := inventory.BootPath(inv, rules, "exe2x"); err == nil {
		t.Error("a node with no boot path should be reported")
	}
}
