// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// serveDHCP answers a read of a DHCP configuration file with the content
// given for its path, reports every link of a boot link script as made, and
// answers every other request with an empty success.
func serveDHCP(files map[string]string) *transport.Recorder {
	return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if len(req.Argv) == 2 && req.Argv[0] == "cat" {
			if content, ok := files[req.Argv[1]]; ok {
				return &transport.Result{Target: tg, Stdout: content}, nil
			}
			return &transport.Result{Target: tg, ExitCode: 1, Stderr: "No such file or directory"}, nil
		}
		var report strings.Builder
		for _, line := range strings.Split(req.Script, "\n") {
			if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "bootlink" {
				report.WriteString("ok\t" + fields[1] + "\n")
			}
		}
		return &transport.Result{Target: tg, Stdout: report.String()}, nil
	}}
}

// linkScripts returns the scripts sent to the install host.
func linkScripts(rec *transport.Recorder) string {
	var out []string
	for _, c := range rec.Calls() {
		if c.Request.Script != "" {
			out = append(out, c.Request.Script)
		}
	}
	return strings.Join(out, "\n")
}

const dhcpdPath = "/etc/dhcp/dhcpd.conf"

// A declaration closed on the line of its last statement swallowed the next
// declaration, whose address then went to the first node (report 5.2).
func TestBootSetReadsADeclarationClosedOnItsLastLine(t *testing.T) {
	conf := `
host exe0003 {
  hardware ethernet aa:bb:cc:00:00:03;
  fixed-address 10.0.2.3; }
host exe0004 {
  hardware ethernet aa:bb:cc:00:00:04;
  fixed-address 10.0.2.4;
}
`
	rec := serveDHCP(map[string]string{dhcpdPath: conf})
	if _, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-y", "-n", "exe0003"); err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	if script := linkScripts(rec); !strings.Contains(script, " /srv/pxesrv/10.0.2.3\n") {
		t.Errorf("boot set linked the wrong address:\n%s", script)
	}

	h, err := run(t, harnessOptions{recorder: serveDHCP(map[string]string{dhcpdPath: conf})},
		"dhcp", "hosts", "-n", "exe[0003-0004]", "-o", "json")
	if err != nil {
		t.Fatalf("dhcp hosts failed: %v\n%s", err, h.out.String())
	}
	out := h.out.String()
	for _, want := range []string{`"10.0.2.4"`, `"aa:bb:cc:00:00:04"`} {
		if !strings.Contains(out, want) {
			t.Errorf("dhcp hosts is missing %s:\n%s", want, out)
		}
	}
	if strings.Count(out, "aa:bb:cc:00:00:04") != 1 {
		t.Errorf("exe0004's MAC is reported more than once:\n%s", out)
	}
}

// An include statement is followed, so the declarations in the included file
// are known.
func TestDHCPHostsFollowsInclude(t *testing.T) {
	rec := serveDHCP(map[string]string{
		dhcpdPath: "include \"/etc/dhcp/hosts.conf\";\n",
		"/etc/dhcp/hosts.conf": "host exe0008 { hardware ethernet aa:bb:cc:00:00:08; " +
			"fixed-address 10.0.2.8; }\n",
	})
	h, err := run(t, harnessOptions{recorder: rec}, "dhcp", "hosts", "-n", "exe0008")
	if err != nil {
		t.Fatalf("dhcp hosts failed: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "10.0.2.8") {
		t.Errorf("dhcp hosts did not read the included file:\n%s", out)
	}
}

// A file that cannot be parsed stops the command instead of reporting
// whatever was read up to the fault.
func TestDHCPHostsRefusesAHostInsideAHost(t *testing.T) {
	rec := serveDHCP(map[string]string{dhcpdPath: `
host exe0003 {
  fixed-address 10.0.2.3;
host exe0004 {
  fixed-address 10.0.2.4;
}
}
`})
	_, err := run(t, harnessOptions{recorder: rec}, "dhcp", "hosts", "-n", "exe0003")
	if got := exitcode.From(err); got != exitcode.TargetFailed {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitcode.TargetFailed, err)
	}
	if !strings.Contains(err.Error(), "exe0004") {
		t.Errorf("error = %v, want it to name the nested declaration", err)
	}
}
