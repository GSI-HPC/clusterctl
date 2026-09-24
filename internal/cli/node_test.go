// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// inventoryPositions finds the file:line references to the example
// inventory in an error.
var inventoryPositions = regexp.MustCompile(`inventory\.yaml:\d+`)

// Report 4.3: exe1 written after exe[0001-0010] made a second record for
// exe0001 that no selection reached, so its class, its address and everything
// else it said were silently ignored. It names the same host as exe0001, and which of the
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

// Report 4.14: an address copied from exe0001 onto exe0002 made a reinstall of
// exe0002 point exe0001's PXE link at another boot image.
func TestInventoryRefusesACopiedAddress(t *testing.T) {
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0002\n      address: 10.0.2.1\n"
	})
	_, err := run(t, harnessOptions{config: []string{inventory}}, "config", "validate")
	if err == nil {
		t.Fatal("config validate accepted two nodes with one address")
	}
	if !strings.Contains(err.Error(), "10.0.2.1") {
		t.Errorf("error = %v, want it to name the address", err)
	}
	if got := inventoryPositions.FindAllString(err.Error(), -1); len(got) != 2 || got[0] == got[1] {
		t.Errorf("error = %v, want the file and line of both entries", err)
	}

	h, err := run(t, harnessOptions{config: []string{inventory}}, "provision", "reinstall", "-y", "-n", "exe0002")
	if err == nil {
		t.Fatal("a reinstall went ahead on an inventory with a copied address")
	}
	if n := len(h.recorder.Calls()); n != 0 {
		t.Errorf("%d commands were sent", n)
	}
}

// An address that is not an IP address went straight into the name of a
// link on the PXE server.
func TestInventoryRefusesAnAddressThatIsNotAnIPAddress(t *testing.T) {
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return strings.Replace(s, "address: 10.0.2.1", "address: ../../etc/x", 1)
	})
	_, err := run(t, harnessOptions{config: []string{inventory}}, "config", "validate")
	if err == nil {
		t.Fatal("config validate accepted a path as an address")
	}
	if !strings.Contains(err.Error(), "../../etc/x") || len(inventoryPositions.FindAllString(err.Error(), -1)) != 1 {
		t.Errorf("error = %v, want the value and where it was written", err)
	}
}

// Report 4.15: a refinement moving exe0001 to another rack as an attribute was
// reverted to the rack it inherited, so a power action on the old rack still
// reached it.
func TestRackAttributeIsNotOverwritten(t *testing.T) {
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0001\n      attributes: {rack: R05}\n"
	})
	h, err := run(t, harnessOptions{config: []string{inventory}}, "node", "select", "@rack:R02")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0002-0010]"; got != want {
		t.Errorf("@rack:R02 = %q, want %q", got, want)
	}
	h, err = run(t, harnessOptions{config: []string{inventory}}, "node", "rack", "R05")
	if err != nil {
		t.Fatalf("node rack R05: %v", err)
	}
	if !strings.Contains(h.out.String(), "exe0001") {
		t.Errorf("node rack R05 does not list exe0001:\n%s", h.out)
	}
}

// Report 4.17: node describe exe1 found exe0001 but showed host and service
// processor names built from exe1, names no acting command uses.
func TestNodeDescribeUsesTheInventoryName(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "describe", "exe1", "-o", "json")
	if err != nil {
		t.Fatalf("node describe failed: %v", err)
	}
	var got struct {
		Node struct{ Name string } `json:"node"`
		Host string                `json:"host"`
		BMC  string                `json:"bmc"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", h.out, err)
	}
	if got.Node.Name != "exe0001" || got.Host != "exe0001.hpc.example.org" || got.BMC != "exe0001.mgmt.hpc.example.org" {
		t.Errorf("node describe exe1 = %+v, want every name built from exe0001", got)
	}
	for _, call := range h.recorder.Calls() {
		if strings.Contains(call.Command, "exe1") && !strings.Contains(call.Command, "exe0001") {
			t.Errorf("a group source was asked about exe1: %q", call.Command)
		}
	}
}

// Report 12.8: node hw called every failed node unreachable and left it out of
// -o json altogether, so a script saw fewer nodes and no failure.
func TestNodeHardwareReportsEveryFailedNode(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		switch tg.Name {
		case "exe0002":
			return &transport.Result{Target: tg, ExitCode: 2, Stderr: "lspci: \x1b[31mdenied\n",
				Err: errors.New("exit status 2")}, nil
		case "exe0003":
			return &transport.Result{Target: tg, ExitCode: -1,
				Err: exitcode.Errorf(exitcode.Transport, "ssh: connect to host exe0003: No route to host")}, nil
		}
		return &transport.Result{Target: tg, Stdout: "Vendor|Model|BV|BN|BIOSV|1.2|2026-01-01|MT4123\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "node", "hw", "-n", "exe[1-3]", "-o", "json")
	if err == nil {
		t.Fatal("node hw with failed nodes should fail")
	}
	var entries []map[string]string
	if err := json.Unmarshal(h.out.Bytes(), &entries); err != nil {
		t.Fatalf("decoding %s: %v", h.out, err)
	}
	byNode := map[string]map[string]string{}
	for _, e := range entries {
		byNode[e["node"]] = e
	}
	if len(byNode) != 3 {
		t.Fatalf("-o json has %d nodes, want all 3:\n%s", len(byNode), h.out)
	}
	if e := byNode["exe0001"]; e["status"] != "ok" || e["vendor"] != "Vendor" {
		t.Errorf("exe0001 = %v, want ok and its vendor", e)
	}
	if e := byNode["exe0002"]; e["status"] != "exit 2" || !strings.Contains(e["error"], "lspci") {
		t.Errorf("exe0002 = %v, want exit 2 and the error it printed", e)
	}
	if e := byNode["exe0003"]; e["status"] != "unreachable" || !strings.Contains(e["error"], "No route to host") {
		t.Errorf("exe0003 = %v, want unreachable and why", e)
	}

	// The table says the same, and does not pass a node's control
	// characters to the terminal.
	h, _ = run(t, harnessOptions{recorder: rec}, "node", "hw", "-n", "exe[1-3]")
	out := h.out.String() + h.errOut.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("a control character reached the terminal:\n%q", out)
	}
	for _, want := range []string{"exit 2", `lspci: \x1b[31mdenied`, "No route to host"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(h.out.String(), "exe0002  unreachable") {
		t.Errorf("exe0002 is called unreachable:\n%s", h.out)
	}
}
