// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package inventory_test

import (
	"errors"
	"net"
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
	if got, want := inv.InRack("R02").String(), "exe[1-4]"; got != want {
		t.Errorf("InRack = %q, want %q", got, want)
	}
	if got, want := inv.NodeSet().String(), "exe[1-4],wlm01"; got != want {
		t.Errorf("NodeSet = %q, want %q", got, want)
	}
	if got, want := len(inv.All()), 5; got != want {
		t.Errorf("All returned %d nodes, want %d", got, want)
	}
}

func TestLookupIgnoresPadding(t *testing.T) {
	t.Parallel()

	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[0001-0004]", Attributes: map[string]string{"class": "exe"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// An administrator types exe1; the inventory wrote exe0001.
	node, ok := inv.Lookup("exe1")
	if !ok {
		t.Fatal("exe1 did not find exe0001")
	}
	if got, want := node.Name, "exe0001"; got != want {
		t.Errorf("Lookup(exe1).Name = %q, want %q", got, want)
	}
	known, unknown := inv.Select(nodeset.MustParse("exe[1-2],exe9"))
	if got, want := len(known), 2; got != want {
		t.Errorf("Select found %d nodes, want %d", got, want)
	}
	if got, want := strings.Join(unknown, ","), "exe9"; got != want {
		t.Errorf("unknown = %q, want %q", got, want)
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

// Report 4.3: exe1 and exe0001 are one host, so an entry spelling a host
// another entry already named with other padding would either make a second,
// phantom record or silently merge into the first. Which one the author meant
// cannot be told, so the inventory is refused.
func TestNamesDifferingOnlyInPaddingAreRejected(t *testing.T) {
	t.Parallel()

	cases := map[string][]v1alpha1.NodeEntry{
		"a short refinement of a padded range": {
			{Nodes: "exe[0001-0010]", Attributes: map[string]string{"class": "exe"}},
			{Nodes: "exe1", Attributes: map[string]string{"class": "spare"}},
			{Nodes: "exe2", Address: "10.0.2.2"},
		},
		"a padded refinement of a short range": {
			{Nodes: "exe[1-10]"},
			{Nodes: "exe0001", Address: "10.0.2.1"},
		},
		"a range written again with other padding": {
			{Nodes: "exe[01-02]"},
			{Nodes: "exe[1-2]"},
		},
		// Host names are not case sensitive, so WLM01 is wlm01 too.
		"names differing only in case": {
			{Nodes: "wlm01"},
			{Nodes: "WLM01", Address: "10.0.1.1"},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: entries})
			if err == nil {
				t.Fatal("two spellings of one host should be rejected")
			}
			if !strings.Contains(err.Error(), "entry 1") || !strings.Contains(err.Error(), "entry 2") {
				t.Errorf("error = %v, want it to name both entries", err)
			}
		})
	}
}

// A refinement written the way the host was first written is still a
// refinement, and hosts whose names differ in more than padding are distinct
// even when their widths differ.
func TestMixedWidthsOfDistinctHostsAreAccepted(t *testing.T) {
	t.Parallel()

	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[0001-0010]", Attributes: map[string]string{"class": "exe"}},
		{Nodes: "exe11", Attributes: map[string]string{"class": "exe"}},
		{Nodes: "exe0001", Attributes: map[string]string{"class": "spare"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := inv.Len(), 11; got != want {
		t.Errorf("Len = %d, want %d", got, want)
	}
	if got, want := inv.WithAttribute("class", "spare").String(), "exe0001"; got != want {
		t.Errorf("@spare = %q, want %q", got, want)
	}
	if got, want := inv.WithAttribute("class", "exe").String(), "exe[0002-0010,11]"; got != want {
		t.Errorf("@exe = %q, want %q", got, want)
	}
}

// Report 4.6: with exe11 next to exe[0001-0010], selecting any of them must
// return real nodes, never a nil one for the caller to dereference.
func TestSelectNeverReturnsANilNode(t *testing.T) {
	t.Parallel()

	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[0001-0010]"},
		{Nodes: "exe11"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{"exe11", "exe[1-11]", "exe[0001-0011]", "exe[01-11]"} {
		known, unknown := inv.Select(nodeset.MustParse(expr))
		for _, n := range known {
			if n == nil {
				t.Fatalf("Select(%s) returned a nil node", expr)
			}
		}
		if len(known)+len(unknown) != nodeset.MustParse(expr).Len() {
			t.Errorf("Select(%s) lost a name: %d known, %v unknown", expr, len(known), unknown)
		}
	}
	known, _ := inv.Select(nodeset.MustParse("exe11"))
	if len(known) != 1 || known[0].Name != "exe11" {
		t.Errorf("Select(exe11) = %v, want exe11", known)
	}
}

// Report 4.18: looking up a name must not rebuild the set of every node, or
// selecting a large group costs quadratic time.
func TestLookupDoesNotScaleWithTheInventory(t *testing.T) {
	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[0001-4000]"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ns := nodeset.MustParse("exe1")
	for name, f := range map[string]func(){
		"Resolve": func() { _, _ = inv.Resolve("exe1") },
		"Lookup":  func() { _, _ = inv.Lookup("exe1") },
		"Select":  func() { _, _ = inv.Select(ns) },
	} {
		// Rebuilding the set allocates for each of the 4,000 nodes.
		if allocs := testing.AllocsPerRun(10, f); allocs > 100 {
			t.Errorf("%s allocates %.0f times for one name in 4,000", name, allocs)
		}
	}
}

// Report 4.14: a copied address, MAC, cid or service processor address sends
// an action meant for one machine to another, and an address that is not an
// IP address ends up in the name of a file on the PXE server.
func TestMachineIdentifiersMustBeUniqueAndWellFormed(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		entries []v1alpha1.NodeEntry
		want    string
	}{
		"a shared address": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", Address: "10.0.2.1"},
			{Nodes: "exe2", Address: "10.0.2.1"},
		}, "10.0.2.1"},
		"a shared MAC, written differently": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", MACs: []string{"00:11:22:33:44:55"}},
			{Nodes: "exe2", MACs: []string{"00:11:22:33:44:66", "00-11-22-33-44-55"}},
		}, "00:11:22:33:44:55"},
		"a shared cid": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", CID: "123"},
			{Nodes: "exe2", CID: "123"},
		}, "123"},
		"a shared service processor": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", BMCAddress: "10.9.0.1"},
			{Nodes: "exe2", BMCAddress: "10.9.0.1"},
		}, "10.9.0.1"},
		"a service processor at a node's address": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", Address: "10.0.2.1"},
			{Nodes: "exe2", BMCAddress: "10.0.2.1"},
		}, "10.0.2.1"},
		"an address with a prefix length": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", Address: "10.0.2.1/24"},
		}, "10.0.2.1/24"},
		"an address that is a path": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", Address: "../../etc/x"},
		}, "../../etc/x"},
		"an address with a zone": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", Address: "fe80::1%eth0"},
		}, "fe80::1%eth0"},
		"a malformed MAC": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", MACs: []string{"00:11:22:33:44"}},
		}, "00:11:22:33:44"},
		"a service processor name that is not a host name": {[]v1alpha1.NodeEntry{
			{Nodes: "exe1", BMCAddress: "bmc/../x"},
		}, "bmc/../x"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: tc.entries})
			if err == nil {
				t.Fatal("the inventory should be rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(tc.entries) > 1 && (!strings.Contains(err.Error(), "entry 1") || !strings.Contains(err.Error(), "entry 2")) {
				t.Errorf("error = %v, want it to name both entries", err)
			}
		})
	}
}

// Uniqueness is judged on what the inventory ends up holding: a refinement
// that moves an address away frees it for another node.
func TestARefinedAddressIsFreeAgain(t *testing.T) {
	t.Parallel()

	_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe1", Address: "10.0.2.1", BMCAddress: "exe1-bmc.example.org"},
		{Nodes: "exe1", Address: "10.0.2.11"},
		{Nodes: "exe2", Address: "10.0.2.1", BMCAddress: "2001:db8::2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// Report 4.15: an attribute an entry sets is not overwritten by the rack it
// inherited, and a rack written only as an attribute is still a rack.
func TestRackAttributeAndFieldAgree(t *testing.T) {
	t.Parallel()

	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[0001-0004]", Rack: "R02", Level: "1"},
		{Nodes: "exe0001", Attributes: map[string]string{"rack": "R05", "level": "3"}},
		{Nodes: "sub0001", Attributes: map[string]string{"rack": "R07"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	exe1, _ := inv.Lookup("exe0001")
	if exe1.Attributes["rack"] != "R05" || exe1.Rack != "R05" {
		t.Errorf("exe0001: rack attribute %q, field %q, want R05 for both", exe1.Attributes["rack"], exe1.Rack)
	}
	if exe1.Attributes["level"] != "3" || exe1.Level != "3" {
		t.Errorf("exe0001: level attribute %q, field %q, want 3 for both", exe1.Attributes["level"], exe1.Level)
	}
	if got, want := inv.WithAttribute("rack", "R02").String(), "exe[0002-0004]"; got != want {
		t.Errorf("@rack:R02 = %q, want %q", got, want)
	}
	if got, want := inv.InRack("R02").String(), "exe[0002-0004]"; got != want {
		t.Errorf("InRack(R02) = %q, want %q", got, want)
	}
	if got, want := inv.InRack("R07").String(), "sub0001"; got != want {
		t.Errorf("InRack(R07) = %q, want %q", got, want)
	}
	if got, want := strings.Join(inv.Racks(), ","), "R02,R05,R07"; got != want {
		t.Errorf("Racks = %q, want %q", got, want)
	}
	// A rack is matched the way the @rack: group matches it.
	if got := inv.InRack("r07").String(); got != "" {
		t.Errorf("InRack(r07) = %q, want nothing, as @rack:r07", got)
	}
}

// An entry that says two different things about the rack is a mistake.
func TestRackFieldAndAttributeMustNotConflict(t *testing.T) {
	t.Parallel()

	_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe1", Rack: "R02", Attributes: map[string]string{"rack": "R05"}},
	}})
	if err == nil {
		t.Fatal("an entry setting two racks should be rejected")
	}
	if !strings.Contains(err.Error(), "R02") || !strings.Contains(err.Error(), "R05") {
		t.Errorf("error = %v, want it to name both racks", err)
	}
	if _, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe1", Rack: "R02", Attributes: map[string]string{"rack": "R02"}},
	}}); err != nil {
		t.Errorf("an entry saying the same rack twice is consistent: %v", err)
	}
}

// A malformed identifier's error keeps the parser's error underneath it, so
// that a caller can still tell what went wrong.
func TestMalformedIdentifierKeepsTheParseError(t *testing.T) {
	t.Parallel()

	_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe1", MACs: []string{"00:11:22:33:44"}},
	}})
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("error = %v, want the *net.AddrError of the MAC parser wrapped in it", err)
	}
}

// Every lookup lowercases the name it is given, so a node kept as EXE0001
// was never found, and lost its bmcAddress to the naming rules.
func TestNamesWithCapitalsAreRejected(t *testing.T) {
	t.Parallel()

	for _, nodes := range []string{"EXE0001", "exe[0001-0002],Wlm01"} {
		_, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{{Nodes: nodes}}})
		if err == nil {
			t.Errorf("%s: accepted, want capitals refused", nodes)
			continue
		}
		if !strings.Contains(err.Error(), "write it") {
			t.Errorf("%s: error = %v, want the spelling to use", nodes, err)
		}
	}
}
