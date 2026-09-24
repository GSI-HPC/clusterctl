// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// exampleWith writes a copy of an example document, changed by edit, into a
// directory the harness reads after the example, where it replaces the
// document of the same kind and name.
func exampleWith(t *testing.T, file string, edit func(string) string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(exampleDir, file))
	if err != nil {
		t.Fatal(err)
	}
	changed := edit(string(data))
	if changed == string(data) {
		t.Fatalf("the edit left %s unchanged", file)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ipmiHosts returns the --hostname arguments the recorded IPMI runs named.
func ipmiHosts(h *harness) []string {
	var hosts []string
	for _, call := range h.recorder.Calls() {
		fields := strings.Fields(call.Command)
		for i, f := range fields {
			if f == "--hostname" && i+1 < len(fields) {
				hosts = append(hosts, strings.Trim(fields[i+1], `\'"`))
			}
		}
	}
	return hosts
}

// ipmiAnswer builds a reply for the recording transport that answers every
// IPMI run the way a working backend does, with answer for each processor
// the run names, and succeeds for everything else.
func ipmiAnswer(answer func(bmc string) string) func(transport.Target, transport.Request) (*transport.Result, error) {
	return func(target transport.Target, req transport.Request) (*transport.Result, error) {
		var out strings.Builder
		for _, bmc := range ipmiRequestHosts(req) {
			fmt.Fprintf(&out, "%s: %s\n", bmc, answer(bmc))
		}
		return &transport.Result{Target: target, Stdout: out.String()}, nil
	}
}

// ipmiOK answers every IPMI run with success: "ok" for an action and "on" for
// a status.
func ipmiOK() *transport.Recorder {
	return &transport.Recorder{Reply: func(target transport.Target, req transport.Request) (*transport.Result, error) {
		if isSinfo(req) {
			return sinfoAllIdle(target, req), nil
		}
		state := "ok"
		switch {
		case strings.Contains(req.Script, "--stat"):
			state = "on"
		case strings.Contains(req.Script, "chassis power status"):
			state = "Chassis Power is on"
		case strings.Contains(req.Script, "chassis power "):
			for action, text := range map[string]string{
				"on": "Up/On", "off": "Down/Off", "cycle": "Cycle", "reset": "Reset", "soft": "Soft",
			} {
				if strings.Contains(req.Script, "chassis power "+action+" ") {
					state = "Chassis Power Control: " + text
				}
			}
		}
		return ipmiAnswer(func(string) string { return state })(target, req)
	}}
}

// sinfoAllIdle answers sinfo with every node it was asked about idle.
func sinfoAllIdle(target transport.Target, req transport.Request) *transport.Result {
	var out strings.Builder
	for i, arg := range req.Argv {
		if arg != "-n" || i+1 == len(req.Argv) {
			continue
		}
		if ns, err := nodeset.Parse(req.Argv[i+1]); err == nil {
			for _, name := range ns.Expand() {
				fmt.Fprintf(&out, "%s idle\n", name)
			}
		}
	}
	return &transport.Result{Target: target, Stdout: out.String()}
}

// ipmiRequestHosts returns the processors an IPMI run names.
func ipmiRequestHosts(req transport.Request) []string {
	fields := strings.Fields(req.Script)
	for i, f := range fields {
		if f == "--hostname" && i+1 < len(fields) {
			ns, err := nodeset.Parse(strings.Trim(fields[i+1], `'"`))
			if err != nil {
				return nil
			}
			return ns.Expand()
		}
		if f == "for" && i+2 < len(fields) && fields[i+1] == "h" && fields[i+2] == "in" {
			var hosts []string
			for _, h := range fields[i+3:] {
				if h == "do" || strings.HasSuffix(h, ";") {
					hosts = append(hosts, strings.Trim(strings.TrimSuffix(h, ";"), `'`))
					break
				}
				hosts = append(hosts, strings.Trim(h, `'`))
			}
			return hosts
		}
	}
	return nil
}

// With a bmc template and a bmcAddress, the derived name won, so a stale DNS
// record sent the power action to another device.
func TestBMCPowerUsesTheInventoryAddress(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	})

	h, err := run(t, harnessOptions{config: []string{inventory}, recorder: ipmiOK()},
		"bmc", "power", "cycle", "--ipmi", "-y", "-n", "exe[0003-0004]")
	if err != nil {
		t.Fatalf("bmc power failed: %v\n%s", err, h.errOut)
	}
	hosts := strings.Join(ipmiHosts(h), " ")
	if !strings.Contains(hosts, "10.9.0.77") || strings.Contains(hosts, "exe0003") {
		t.Errorf("IPMI was sent to %q, want exe0003 reached at 10.9.0.77", hosts)
	}
	if !strings.Contains(hosts, "exe0004.mgmt.hpc.example.org") {
		t.Errorf("IPMI was sent to %q, want exe0004 reached by its derived name", hosts)
	}

	// node fqdn --bmc shows the same hosts the commands use.
	h, err = run(t, harnessOptions{config: []string{inventory}}, "node", "fqdn", "--bmc", "-n", "exe0003")
	if err != nil {
		t.Fatalf("node fqdn --bmc failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "10.9.0.77"; got != want {
		t.Errorf("node fqdn --bmc -n exe0003 = %q, want %q", got, want)
	}
}

// config init writes one rule without a bmc template. The node's own name
// then became its BMC, and the site's BMC account went to the node.
func TestBMCRefusesANodeWithoutAServiceProcessorName(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	site := exampleWith(t, "site.yaml", func(s string) string {
		s = strings.Replace(s, "        bmc: \"{name}.{domains.mgmtHpc}\"\n", "", 1)
		return strings.Replace(s, "        bmc: \"{bmcPrefix}{name}.{domains.mgmt}\"\n", "", 1)
	})

	for _, args := range [][]string{
		{"bmc", "power", "cycle", "--ipmi", "-y", "-n", "exe0003"},
		{"bmc", "status", "--ipmi", "-n", "exe[0001-0002]"},
		// Redfish would have sent the account over HTTP Basic auth.
		{"bmc", "status", "-n", "exe0001"},
		{"node", "fqdn", "--bmc", "-n", "exe[0001-0002]"},
	} {
		h, err := run(t, harnessOptions{config: []string{site}}, args...)
		if err == nil {
			t.Errorf("%s: accepted, output:\n%s", strings.Join(args, " "), h.out)
			continue
		}
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%s: exit code = %d, want %d (%v)", strings.Join(args, " "), got, exitcode.Usage, err)
		}
		if !strings.Contains(err.Error(), "bmcAddress") {
			t.Errorf("%s: error = %v, want it to say how to name the service processor", strings.Join(args, " "), err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("%s: sent %d commands, want none", strings.Join(args, " "), len(calls))
		}
	}

	// Describing the node still works, and says why it has no BMC.
	h, err := run(t, harnessOptions{config: []string{site}}, "node", "describe", "exe0001")
	if err != nil {
		t.Fatalf("node describe failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "no bmc template") {
		t.Errorf("node describe does not say why there is no BMC:\n%s", h.errOut)
	}

	// A node the inventory gives an address is still reached.
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	})
	h, err = run(t, harnessOptions{config: []string{site, inventory}, recorder: ipmiOK()},
		"bmc", "power", "cycle", "--ipmi", "-y", "-n", "exe0003")
	if err != nil {
		t.Fatalf("bmc power with a bmcAddress failed: %v\n%s", err, h.errOut)
	}
	if got := ipmiHosts(h); len(got) != 1 || got[0] != "10.9.0.77" {
		t.Errorf("IPMI was sent to %q, want 10.9.0.77", got)
	}
}

// Naming ignored case, cut a dotted name at its first dot whatever the
// domain, and did the same to an IP address, which named one host for a
// whole subnet.
func TestBMCNamesTheMachineThatWasMeant(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")

	h, err := run(t, harnessOptions{}, "node", "fqdn", "-n", "WLM01")
	if err != nil {
		t.Fatalf("node fqdn failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "wlm01.hpc.example.org"; got != want {
		t.Errorf("node fqdn -n WLM01 = %q, want %q", got, want)
	}

	for _, nodes := range []string{
		// The rules give login01 the host name login01.example.org.
		"login01.hpc.example.org",
		"10.0.2.[1-4]",
	} {
		h, err := run(t, harnessOptions{}, "bmc", "power", "cycle", "--ipmi", "-y", "--force", "-n", nodes)
		if err == nil {
			t.Errorf("-n %s: accepted, IPMI sent to %q", nodes, ipmiHosts(h))
			continue
		}
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("-n %s: exit code = %d, want %d (%v)", nodes, got, exitcode.Usage, err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("-n %s: sent %d commands, want none", nodes, len(calls))
		}
	}
}

// A vendor profile with its own account and transport order was dropped for
// every spelling but the inventory's, while the command still reached the
// node's service processor with the site's account.
func TestBMCKeepsTheVendorProfileForEverySpelling(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	t.Setenv("VENDOR2_PASSWORD", "v3ndor")
	site := exampleWith(t, "site.yaml", func(s string) string {
		s = strings.Replace(s, "  credentials:\n",
			"  credentials:\n    vendor2:\n      username: v2admin\n      password:\n        fromEnv: VENDOR2_PASSWORD\n", 1)
		s = strings.Replace(s, "      vendor2:\n",
			"      vendor2:\n        credential: vendor2\n        order: [ipmi]\n", 1)
		// ipmitool takes the user name as an argument, so the recorded
		// command shows which account was used.
		return strings.Replace(s, "backend: ipmipower", "backend: ipmitool", 1)
	})

	for _, spelling := range []string{"exe0001", "exe0001.hpc.example.org", "EXE0001", "exe0001."} {
		h, err := run(t, harnessOptions{config: []string{site}, recorder: slurmAllIdle(t)},
			"bmc", "power", "off", "--dry-run", "-n", spelling)
		if err != nil {
			t.Fatalf("-n %s: dry run failed: %v", spelling, err)
		}
		if preview := h.errOut.String() + h.out.String(); !strings.Contains(preview, "through IPMI") {
			t.Errorf("-n %s: the preview does not use the vendor's order:\n%s", spelling, preview)
		}

		h, err = run(t, harnessOptions{config: []string{site}, recorder: ipmiOK()}, "bmc", "power", "off", "--ipmi", "-y", "-n", spelling)
		if err != nil {
			t.Fatalf("-n %s: bmc power failed: %v", spelling, err)
		}
		// The Slurm check runs first; the IPMI run is the last call.
		calls := h.recorder.Calls()
		command := calls[len(calls)-1].Command
		if !strings.Contains(command, "chassis power") {
			t.Fatalf("-n %s: the last command is not the IPMI run:\n%s", spelling, command)
		}
		if !strings.Contains(command, "v2admin") {
			t.Errorf("-n %s: IPMI did not use the vendor's account:\n%s", spelling, command)
		}
		if !strings.Contains(command, "exe0001.mgmt.hpc.example.org") {
			t.Errorf("-n %s: IPMI was not sent to exe0001's service processor:\n%s", spelling, command)
		}
	}
}
