// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// serveDHCP answers a read of a DHCP configuration file with the content
// given for its path, reports every link of a boot link script as made, and
// answers every other request with an empty success.
func serveDHCP(files map[string]string) *transport.Recorder {
	log := &linkLog{}
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if len(req.Argv) == 2 && req.Argv[0] == "cat" {
			if content, ok := files[req.Argv[1]]; ok {
				return &transport.Result{Target: tg, Stdout: content}, nil
			}
			return &transport.Result{Target: tg, ExitCode: 1, Stderr: "No such file or directory"}, nil
		}
		if req.Script == bootLinkScript {
			var report strings.Builder
			for _, r := range linkRecordsOf(req, 3) {
				log.add(strings.Join(r, " "))
				report.WriteString("ok\t" + r[0] + "\n")
			}
			return &transport.Result{Target: tg, Stdout: report.String()}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	linkLogs.Store(rec, log)
	return rec
}

// linkLog keeps the links the boot link scripts of one recorder were given,
// which travel on standard input and are gone from the calls it records.
type linkLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *linkLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

// linkLogs holds the log of each recorder serveDHCP made.
var linkLogs sync.Map

// linkScripts returns the scripts sent to the install host, followed by the
// links they were given, "INDEX TARGET LINK" on a line each.
func linkScripts(rec *transport.Recorder) string {
	var out []string
	for _, c := range rec.Calls() {
		if c.Request.Script != "" {
			out = append(out, c.Request.Script)
		}
	}
	if log, ok := linkLogs.Load(rec); ok {
		log := log.(*linkLog)
		log.mu.Lock()
		for _, line := range log.lines {
			out = append(out, line+"\n")
		}
		log.mu.Unlock()
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
	if command := calls[0].Command; !strings.Contains(command, "'/var/log/dhcp;touch /tmp/pwned'") {
		t.Errorf("the log path is not quoted: %s", command)
	}
	if script := calls[0].Request.Argv[2]; strings.Contains(script, "pwned") {
		t.Errorf("the log path is spliced into the script: %s", script)
	}
}

// dhcp log ran grep | tail, which exits with tail's status, so a log that
// could not be read, such as the default /var/log/syslog on a host that logs
// to the journal alone, printed nothing and exited 0, as a log without a
// DHCP line does. The command runs in a real shell against a log in a
// temporary directory.
func TestDHCPLogSaysWhenTheLogCannotBeRead(t *testing.T) {
	log := filepath.Join(t.TempDir(), "syslog")
	mustWrite(t, log, `Sep 25 10:00:01 dhcp01 dhcpd[11]: DHCPDISCOVER from aa:bb:cc:00:00:01 via ib0
Sep 25 10:00:02 dhcp01 cron[7]: (root) CMD (true)
Sep 25 10:00:03 dhcp01 dhcpd[11]: DHCPOFFER on 10.0.2.1 to aa:bb:cc:00:00:01 via ib0
Sep 25 10:00:04 dhcp01 dhcpd[11]: DHCPACK on 10.0.2.1 to aa:bb:cc:00:00:01 via ib0
`)
	p := newPXEHost(t, pxeOptions{})
	logAt := func(path string, args ...string) (*harness, error) {
		return p.run(t, harnessOptions{}, append([]string{"--set", "services.dhcp.logPath=" + path, "dhcp", "log"}, args...)...)
	}

	h, err := logAt(log, "--lines", "2")
	if err != nil {
		t.Fatalf("dhcp log: %v\n%s", err, h.errOut)
	}
	if got, want := h.out.String(), "Sep 25 10:00:03 dhcp01 dhcpd[11]: DHCPOFFER on 10.0.2.1 to aa:bb:cc:00:00:01 via ib0\n"+
		"Sep 25 10:00:04 dhcp01 dhcpd[11]: DHCPACK on 10.0.2.1 to aa:bb:cc:00:00:01 via ib0\n"; got != want {
		t.Errorf("dhcp log --lines 2 printed\n%s\nwant\n%s", got, want)
	}
	if calls := p.rec.Calls(); len(calls) != 1 || calls[0].Target.Name != "dhcp" {
		t.Errorf("the log was read from %v, want the dhcp role", calls)
	}

	missing := filepath.Join(t.TempDir(), "nosuch")
	h, err = logAt(missing)
	wantCode(t, err, exitcode.TargetFailed)
	if err == nil || !strings.Contains(err.Error(), missing+" cannot be read") {
		t.Errorf("a log that is not there: %v, want it named", err)
	}
	if h.out.Len() != 0 {
		t.Errorf("a log that is not there printed:\n%s", h.out)
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

// --seconds had no upper bound, and time.Duration(seconds)*time.Second
// overflowed: --seconds 10000000000 came out below zero, which is no time
// limit at all, so tcpdump ran as root until something stopped it, the
// very case the lower bound is there to prevent.
func TestDHCPCaptureAlwaysStopsOnItsOwn(t *testing.T) {
	h, err := run(t, harnessOptions{}, "--dry-run", "dhcp", "capture", "--seconds", "3600")
	if err != nil {
		t.Fatalf("dhcp capture --seconds 3600: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "timeout -k 5s 3600s tcpdump") {
		t.Errorf("the capture of an hour does not run under a timeout:\n%s", out)
	}

	for _, seconds := range []string{"3601", "10000000000", "9223372036854775807"} {
		h, err := run(t, harnessOptions{}, "--dry-run", "dhcp", "capture", "--seconds", seconds)
		wantCode(t, err, exitcode.Usage)
		if out := h.out.String(); out != "" {
			t.Errorf("--seconds %s printed a command line:\n%s", seconds, out)
		}
		wantNoCalls(t, h)
	}
}

// roleShell took only what follows -- for the command and dropped the words
// before it, so dhcp shell uptime opened an interactive login on the DHCP
// server instead of running uptime, and boot shell and cinc shell did the
// same. login refuses such a word, and so do they now.
func TestServiceShellRefusesACommandBeforeTheDash(t *testing.T) {
	for _, args := range [][]string{
		{"dhcp", "shell", "uptime"},
		{"boot", "shell", "uptime"},
		{"cinc", "shell", "uptime"},
		{"dhcp", "shell", "uptime", "--", "ls"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{tty: true}, append([]string{"--dry-run"}, args...)...)
			wantCode(t, err, exitcode.Usage)
			if want := "clusterctl " + args[0] + " shell -- uptime"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to show %q", err, want)
			}
			if out := h.out.String(); out != "" {
				t.Errorf("a session was opened:\n%s", out)
			}
			wantNoCalls(t, h)
		})
	}
}

// A service shell reaches the host of the service's role, and runs what
// follows -- there, or opens a login shell when nothing does.
func TestServiceShellReachesTheRoleOfTheService(t *testing.T) {
	for _, tc := range []struct {
		args []string
		host string
		cmd  string
	}{
		{[]string{"dhcp", "shell"}, "root@dhcp01.example.org", ""},
		{[]string{"dhcp", "shell", "--", "ls", "-l", "/etc/dhcp"}, "root@dhcp01.example.org", "sh ls -l /etc/dhcp"},
		{[]string{"boot", "shell", "--", "uptime"}, "installer.hpc.example.org", "sh uptime"},
		{[]string{"cinc", "shell", "--", "uptime"}, "installer.hpc.example.org", "sh uptime"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{tty: true}, append([]string{"--dry-run"}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v", err)
			}
			line := strings.TrimSpace(h.out.String())
			before, after, ok := strings.Cut(line, tc.host)
			if !ok || !strings.HasPrefix(before, "ssh ") {
				t.Fatalf("the session does not reach %s:\n%s", tc.host, line)
			}
			if (tc.cmd == "" && after != "") || !strings.HasSuffix(after, tc.cmd) {
				t.Errorf("the session runs %q, want %q", after, tc.cmd)
			}
			wantNoCalls(t, h)
		})
	}

	_, err := run(t, harnessOptions{tty: true}, "--set", `services.dhcp.role=""`, "--dry-run", "dhcp", "shell")
	wantCode(t, err, exitcode.Usage)
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

// Two nodes the inventory gives no address to, whose DHCP declarations hand
// out one address, were not refused: the owners of an address were looked
// up in the inventory only, so boot set -n exe0005 linked 10.0.9.1 and so
// armed exe0006 as well. A second interface of the node itself holding its
// address is still the node.
func TestBootRefusesAnAddressDHCPHandsToAnotherNode(t *testing.T) {
	shared := `
host exe0005 {
  hardware ethernet aa:bb:cc:00:00:05;
  fixed-address 10.0.9.1;
}
host exe0006 {
  hardware ethernet aa:bb:cc:00:00:06;
  fixed-address 10.0.9.1;
}
`
	for _, args := range [][]string{
		{"boot", "set", "-n", "exe0005"},
		{"boot", "set", "-n", "exe0005", "--dry-run"},
		{"boot", "unset", "-n", "exe0006"},
		{"boot", "grub", "unset", "exe0005"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rec := serveDHCP(map[string]string{dhcpdPath: shared})
			_, err := run(t, harnessOptions{recorder: rec}, append(args, "-y")...)
			wantCode(t, err, exitcode.Usage)
			if err == nil || !strings.Contains(err.Error(), "exe0005") || !strings.Contains(err.Error(), "exe0006") ||
				!strings.Contains(err.Error(), "10.0.9.1") {
				t.Errorf("error = %v, want it to name both nodes and the address", err)
			}
			for _, c := range rec.Calls() {
				if len(c.Request.Argv) == 0 || c.Request.Argv[0] != "cat" {
					t.Errorf("sent %q although the address is shared", c.Command)
				}
			}
		})
	}

	rec := serveDHCP(map[string]string{dhcpdPath: `
host exe0005 {
  hardware ethernet aa:bb:cc:00:00:05;
  fixed-address 10.0.9.1;
}
host exe0005-eth1 {
  hardware ethernet aa:bb:cc:00:01:05;
  fixed-address 10.0.9.1;
}
`})
	if _, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-y", "-n", "exe0005"); err != nil {
		t.Fatalf("boot set refused an address the node's own interfaces share: %v", err)
	}
	if script := linkScripts(rec); !strings.Contains(script, " /srv/pxesrv/10.0.9.1\n") {
		t.Errorf("boot set did not link the node's address:\n%s", script)
	}
}
