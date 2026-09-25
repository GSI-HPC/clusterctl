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
		for line := range strings.SplitSeq(req.Script, "\n") {
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

// A comment above a neighbour's declaration that names the node gave the node
// the neighbour's address, and boot set armed the neighbour (report 5.1).
func TestBootSetIgnoresACommentThatNamesTheNode(t *testing.T) {
	rec := serveDHCP(map[string]string{dhcpdPath: `
# chassis C07: exe0003 exe0004
host exe0003 {
  hardware ethernet aa:bb:cc:00:00:03;
  fixed-address 10.0.2.3;
}
host exe0004 {
  hardware ethernet aa:bb:cc:00:00:04;
  fixed-address 10.0.2.4;
}
`})
	h, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-y", "-n", "exe0004")
	if err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	script := linkScripts(rec)
	if !strings.Contains(script, " /srv/pxesrv/10.0.2.4\n") || strings.Contains(script, "10.0.2.3") {
		t.Errorf("boot set linked the wrong address:\n%s", script)
	}
	if out := h.out.String(); strings.Contains(out, "10.0.2.3") {
		t.Errorf("boot set reported the neighbour's address:\n%s", out)
	}
}

// A node named only in a comment has no address of its own, so nothing is
// linked for it (report 5.1 and 5.3).
func TestProvisionReinstallRefusesANodeNamedOnlyInAComment(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "secret")
	rec := serveDHCP(map[string]string{dhcpdPath: `
# rack R02: exe0001 to exe0010
host exe0002 {
  fixed-address 10.0.2.2;
}
`})
	_, err := run(t, harnessOptions{recorder: rec},
		"provision", "reinstall", "-y", "--no-reset", "-n", "exe0010")
	if err == nil {
		t.Fatal("reinstall of a node with no declaration of its own should fail")
	}
	wantCode(t, err, exitcode.Usage)
	if script := linkScripts(rec); script != "" {
		t.Errorf("reinstall sent a script although no address is known:\n%s", script)
	}
}

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

// A second interface or the BMC sorted before the node's own declaration and
// became its boot address (report 5.3).
func TestBootSetTakesTheAddressOfTheNodesOwnDeclaration(t *testing.T) {
	rec := serveDHCP(map[string]string{dhcpdPath: `
host exe0005.hpc.example.org {
  fixed-address 10.0.2.5;
}
host exe0005-bmc.mgmt.hpc.example.org {
  fixed-address 10.9.2.5;
}
`})
	h, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-y", "-n", "exe0005")
	if err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	if script := linkScripts(rec); !strings.Contains(script, " /srv/pxesrv/10.0.2.5\n") {
		t.Errorf("boot set linked the wrong address:\n%s", script)
	}
	if out := h.out.String(); strings.Contains(out, "10.9.2.5") {
		t.Errorf("boot set reported the BMC's address:\n%s", out)
	}
}

// Two declarations named after the node that both carry an address leave the
// boot address undecided, so nothing is linked (report 5.3).
func TestBootSetRefusesTwoDeclarationsOfTheNode(t *testing.T) {
	rec := serveDHCP(map[string]string{dhcpdPath: `
host exe0006 {
  fixed-address 10.0.2.6;
}
host exe0006.hpc.example.org {
  fixed-address 10.0.2.16;
}
`})
	_, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-y", "-n", "exe0006")
	if err == nil {
		t.Fatal("boot set should refuse a node with two addresses")
	}
	wantCode(t, err, exitcode.Usage)
	for _, want := range []string{"exe0006", "exe0006.hpc.example.org"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
	if script := linkScripts(rec); script != "" {
		t.Errorf("boot set sent a script although the address is ambiguous:\n%s", script)
	}
}

// dhcp hosts shows a declaration found only through a comment, but says so,
// and does not count it as the node's own.
func TestDHCPHostsMarksACommentMatch(t *testing.T) {
	rec := serveDHCP(map[string]string{dhcpdPath: `
# a node named only in the comment: exe0007
host weird-name-0001 {
  fixed-address 10.0.5.1;
}
`})
	h, err := run(t, harnessOptions{recorder: rec}, "dhcp", "hosts", "-n", "exe0007")
	wantCode(t, err, exitcode.TargetFailed)
	out := h.out.String()
	for _, want := range []string{"weird-name-0001", "comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("dhcp hosts is missing %q:\n%s", want, out)
		}
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

// The log path was spliced into sh -c unquoted (report 1.7).
func TestDHCPLogQuotesTheLogPath(t *testing.T) {
	rec := &transport.Recorder{}
	_, err := run(t, harnessOptions{recorder: rec},
		"--set", "services.dhcp.logPath=/var/log/dhcp;touch /tmp/pwned", "dhcp", "log")
	if err != nil {
		t.Fatalf("dhcp log failed: %v", err)
	}
	calls := rec.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	script := strings.Join(calls[0].Request.Argv, " ")
	if !strings.Contains(script, "'/var/log/dhcp;touch /tmp/pwned'") {
		t.Errorf("the log path is not quoted: %s", script)
	}
}

// --lines is bounded, so the server cannot be asked for an unbounded log
// (report 10.3), and --seconds below one no longer drops the time limit
// (report 1.7).
func TestDHCPLogAndCaptureRejectUnboundedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"dhcp", "log", "--lines", "0"},
		{"dhcp", "log", "--lines", "-5"},
		{"dhcp", "log", "--lines", "100000000"},
		{"dhcp", "capture", "--seconds", "0"},
		{"dhcp", "capture", "--seconds", "-1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rec := &transport.Recorder{}
			_, err := run(t, harnessOptions{recorder: rec}, append([]string{"--dry-run"}, args...)...)
			wantCode(t, err, exitcode.Usage)
			if calls := rec.Calls(); len(calls) != 0 {
				t.Errorf("sent %d requests, want none", len(calls))
			}
		})
	}
}

// The log holds what the nodes sent, so a terminal escape in it is shown
// rather than obeyed.
func TestDHCPLogEscapesControlCharacters(t *testing.T) {
	rec := &transport.Recorder{Responses: []*transport.Result{{
		Stdout: "dhcpd: DHCPREQUEST from aa:bb (evil\x1b[2J)\ndhcpd: DHCPACK\ton eth0",
	}}}
	h, err := run(t, harnessOptions{recorder: rec}, "dhcp", "log")
	if err != nil {
		t.Fatalf("dhcp log failed: %v", err)
	}
	out := h.out.String()
	if strings.Contains(out, "\x1b") || !strings.Contains(out, `evil\x1b[2J`) {
		t.Errorf("the escape is not escaped:\n%q", out)
	}
	if !strings.Contains(out, "\n") || !strings.Contains(out, "\t") {
		t.Errorf("newlines and tabs should be kept:\n%q", out)
	}
}
