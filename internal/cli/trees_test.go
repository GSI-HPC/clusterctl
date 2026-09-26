// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// The trees of the commands whose work has the most parts, as a display
// would come to know them: every part under the one it belongs to, the
// plumbing hidden, and the calls of a node under its target.

// secrets push decrypts every file before it asks, by sops for a key of a
// Secret document and by age for a file of its own, and then writes each
// file to the nodes that are still reachable.
func TestSecretsPushReportsItsDecryptionsAndEachFile(t *testing.T) {
	site := secretSite{values: bmcSecret, identities: true}
	dir, keyFile := site.write(t)
	sealed := filepath.Join(dir, "nslcd.keytab.age")
	sealAge(t, keyFile, sealed, "keytab")
	site.secrets = "        - target: /etc/munge/munge.key\n          secretRef: {name: example, key: munge-key}\n" +
		"        - target: /etc/nslcd.keytab\n          source: " + sealed + "\n"
	if err := os.WriteFile(filepath.Join(dir, "override.yaml"), []byte(
		"apiVersion: clusterctl/v1alpha1\nkind: Config\ncontexts:\n  - name: cluster1\n    cluster: cluster1\n"+
			"    overrides:\n      services.cinc.secrets:\n"+site.secrets), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, tree := watch(t)
	_, err := run(t, harnessOptions{ctx: ctx, config: []string{dir}, recorder: &transport.Recorder{Reply: exe0002Unreachable}},
		"secrets", "push", "-n", "exe[1-3]", "-y")
	wantCode(t, err, exitcode.Transport)
	want := `
command secrets push: failed (transport): 1 of 3 nodes failed: exe0002: exe0002 (exe0002.hpc.example.org): ssh: connect to host exe0002 port 22: Connection timed out
  call decrypt DIR/nslcd.keytab.age source=age [hidden]: ok
  call decrypt the Secret example source=sops [hidden]: ok
  step run total=2 limit=24 [fold]: ok
    target exe[0001,0003]: ok
      call ssh node={} host={} timeout=10m0s exit=0: ok
  step run total=3 limit=24 [fold]: failed (transport): 1 of 3 failed: exe0002
    target exe0002: failed (transport): {} ({}): ssh: connect to host {} port 22: Connection timed out
      call ssh node={} host={} timeout=10m0s exit=255: failed (transport): {} ({}): ssh: connect to host {} port 22: Connection timed out
    target exe[0001,0003]: ok
      call ssh node={} host={} timeout=10m0s exit=0: ok
  wait confirm message=write secrets onto 3 hosts: ok
`
	if got := strings.ReplaceAll(tree(), dir, "DIR"); got != want[1:] {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want[1:])
	}
}

// sealAge encrypts content with age to the identity in keyFile, into path.
func sealAge(t *testing.T, keyFile, path, content string) {
	t.Helper()
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(key)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w, err := age.Encrypt(f, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// provision status asks the processors and then the nodes, two steps side
// by side, each counting every node.
func TestProvisionStatusReportsBothHalves(t *testing.T) {
	h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
	h.link(t, "10.0.2.1", h.exePath())
	h.bmcs.down["exe0002"] = true
	ctx, tree := watch(t)
	_, err := h.run(t, harnessOptions{ctx: ctx}, "provision", "status", "-n", "exe[0001-0003]")
	wantCode(t, err, exitcode.Transport)
	want := `
command provision status: failed (transport): where the reinstallation of exe0002 stands is not fully known: exe0002: exe0002.mgmt.hpc.example.org: dial tcp: connection refused
  call credential bmc source=env [hidden]: ok
  call ssh node=install host=installer.hpc.example.org role=install timeout=10m0s exit=0: ok
  step read the power state total=3 limit=8 [fold]: failed (transport): 1 of 3 failed: exe0002
    target exe0002: failed (transport): {}: dial tcp: connection refused
      call redfish host={} method=GET path=/redfish/v1/Systems/1: failed (transport): {}: dial tcp: connection refused
    target exe[0001,0003]: ok
      call redfish host={} method=GET path=/redfish/v1/Systems/1 http=200: ok
  step run total=3 limit=24 [fold]: ok
    target exe[0001-0003]: ok
      call ssh node={} host={} timeout=30s exit=0: ok
`
	if got := tree(); got != want[1:] {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want[1:])
	}
}

// doctor --remote asks each role whether it answers, and the roles with
// tools to look for whether they are there, one call after the other: a
// role that did not answer is asked nothing more.
func TestDoctorRemoteReportsTheCallsOfEachRole(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if tg.Role == "dhcp" {
			return transport.ExitResult(tg, 255, "", "ssh: connect to host dhcp: Connection refused\n"), nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	ctx, tree := watch(t)
	_, _ = run(t, harnessOptions{ctx: ctx, recorder: rec}, "doctor", "--remote")
	// How many checks failed depends on the machine as well, on whether
	// ssh, scp and sops are installed, so the command's own line is only
	// read for how it ended.
	head, calls, _ := strings.Cut(tree(), "\n")
	if !strings.HasPrefix(head, "command doctor: failed (target): ") || !strings.HasSuffix(head, " checks failed") {
		t.Errorf("the command ended %q, want it failed (target) with the count of the checks that failed", head)
	}
	want := `
  call ssh node=db host=dbm01.hpc.example.org role=db timeout=20s exit=0: ok
  call ssh node=dhcp host=dhcp01.example.org role=dhcp timeout=20s exit=255: failed (transport): dhcp (dhcp01.example.org): ssh: connect to host dhcp: Connection refused
  call ssh node=fabric host=ibgw01.example.org role=fabric timeout=20s exit=0: ok
  call ssh node=fabric host=ibgw01.example.org role=fabric timeout=30s exit=0: ok
  call ssh node=ifs host=ifs01.hpc.example.org role=ifs timeout=20s exit=0: ok
  call ssh node=install host=installer.hpc.example.org role=install timeout=20s exit=0: ok
  call ssh node=install host=installer.hpc.example.org role=install timeout=30s exit=0: ok
  call ssh node=login host=login.hpc.example.org role=login timeout=20s exit=0: ok
  call ssh node=login host=login.hpc.example.org role=login timeout=30s exit=0: ok
  call ssh node=mgmt host=mgmt-gw.example.org role=mgmt timeout=20s exit=0: ok
  call ssh node=mgmt host=mgmt-gw.example.org role=mgmt timeout=30s exit=0: ok
  call ssh node=mirror host=mirror.hpc.example.org role=mirror timeout=20s exit=0: ok
  call ssh node=pool host=pool.example.org role=pool timeout=20s exit=0: ok
  call ssh node=tftp host=tftp.example.org role=tftp timeout=20s exit=0: ok
  call ssh node=wlm host=wlm01.hpc.example.org role=wlm timeout=20s exit=0: ok
`
	if calls != want[1:] {
		t.Errorf("progress:\n%s\nwant:\n%s", calls, want[1:])
	}
}

// A dry run resolves its groups for real, since they only read, and only
// records what it would send: the group's command ends as it came back, the
// command it would run ends skipped.
func TestADryRunLooksUpItsGroupsAndRecordsTheRest(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if slices.Contains(req.Argv, "sinfo") {
			return &transport.Result{Target: tg, Stdout: "exe[1-2]\n"}, nil
		}
		t.Errorf("a dry run sent %q to %s", req.Argv, tg)
		return &transport.Result{Target: tg}, nil
	}}
	ctx, tree := watch(t)
	_, err := run(t, harnessOptions{ctx: ctx, recorder: rec}, "node", "hw", "--dry-run", "-n", "@slurm:batch")
	if err != nil {
		t.Fatal(err)
	}
	want := `
command node hw [dry-run]: ok
  call resolve @slurm:batch cache=miss [hidden]: ok
    call ssh node=login host=login.hpc.example.org role=login timeout=10m0s exit=0 [hidden]: ok
  step run total=2 limit=24 [fold]: ok
    target exe[0001-0002]: ok
      call ssh node={} host={} timeout=10m0s [dry-run]: skipped: dry run: not sent
`
	if got := tree(); got != want[1:] {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want[1:])
	}
}

// Progress goes to the sinks of a Bus, never to standard output or
// standard error: a command given a Bus prints the same bytes and fails
// the same way as one given none, with its notes, tables, previews and
// failures, in a dry run, a fan-out, batches and a reinstall.
func TestProgressLeavesTheOutputAlone(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	fakeStagger(t, func(time.Duration) bool { return false })
	fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
		switch processorOf(req) {
		case "exe0002":
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		case "exe0003":
			return answer(req, http.StatusInternalServerError, `{"error":{"message":"busy"}}`), nil
		}
		return answer(req, http.StatusOK, system), nil
	})
	same := func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
	}
	plain := func(args ...string) func(t *testing.T, opts harnessOptions) (*harness, string, error) {
		return func(t *testing.T, opts harnessOptions) (*harness, string, error) {
			h, err := run(t, opts, args...)
			return h, "", err
		}
	}
	answering := func(reply func(transport.Target, transport.Request) (*transport.Result, error), args ...string) func(t *testing.T, opts harnessOptions) (*harness, string, error) {
		return func(t *testing.T, opts harnessOptions) (*harness, string, error) {
			opts.recorder = &transport.Recorder{Reply: reply}
			h, err := run(t, opts, args...)
			return h, "", err
		}
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, opts harnessOptions) (*harness, string, error)
	}{
		{"exec with a node that cannot be reached", answering(exe0002Unreachable, "exec", "-n", "exe[1-3]", "-y", "--", "uptime")},
		{"exec, the answers folded", answering(same, "exec", "--dedup", "-n", "exe[1-4]", "-y", "--", "uptime")},
		{"a dry run", plain("exec", "--dry-run", "-n", "exe[1-3]", "--", "uptime")},
		{"a read in JSON", answering(same, "-o", "json", "node", "hw", "-n", "exe[1-2]")},
		{"a group looked up in a dry run", answering(func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			return &transport.Result{Target: tg, Stdout: "exe[1-2]\n"}, nil
		}, "node", "hw", "--dry-run", "-n", "@slurm:batch")},
		{"bmc status falling back", func(t *testing.T, opts harnessOptions) (*harness, string, error) {
			opts.recorder = ipmiOK()
			h, err := run(t, opts, append(noSlurm, "bmc", "status", "-n", "exe[0001-0004]")...)
			return h, "", err
		}},
		{"bmc power in batches", func(t *testing.T, opts harnessOptions) (*harness, string, error) {
			opts.recorder = &transport.Recorder{Reply: ipmiAnswer(func(bmc string) string {
				if strings.HasPrefix(bmc, "exe0005.") {
					return "connection timeout"
				}
				return "ok"
			})}
			h, err := run(t, opts, append(noSlurm, "bmc", "power", "on", "--ipmi", "--batch", "3", "-y", "-n", "exe[1-9]")...)
			return h, "", err
		}},
		{"a reinstall that fails a reset", func(t *testing.T, opts harnessOptions) (*harness, string, error) {
			h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
			h.bmcs.refuse["exe0002"] = "ForceRestart"
			out, err := h.run(t, opts, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
			return out, h.root, err
		}},
		{"a usage error", plain("node", "fqdn", "-n", "exe[1-")},
		{"a configuration", plain("config", "validate")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			for _, unwatched := range []bool{false, true} {
				h, root, err := tc.run(t, harnessOptions{unwatched: unwatched})
				text := fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nexit %d: %v", h.out, h.errOut, exitcode.From(err), err)
				if root != "" {
					text = strings.ReplaceAll(text, root, "ROOT")
				}
				seen = append(seen, text)
			}
			if seen[0] != seen[1] {
				t.Errorf("with a Bus:\n%s\n\nwithout one:\n%s", seen[0], seen[1])
			}
		})
	}
}
