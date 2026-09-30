// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The sind cluster the suite runs against, as testdata/sind-cluster.yaml
// creates it and testdata/site/ describes it to clusterctl. sind names a
// node <node>.<cluster>.<realm>.sind.
const (
	realm   = "e2e"
	cluster = "alpha"
	domain  = cluster + "." + realm + ".sind"
)

// workers are the nodes slurmd runs on, in the one partition sind makes.
var workers = []string{"worker-0", "worker-1", "worker-2"}

const (
	// commandTimeout bounds one run of clusterctl, so that a hung ssh
	// fails its test rather than the whole run.
	commandTimeout = 2 * time.Minute
	// slurmTimeout bounds the wait for the workers to register with
	// slurmctld after the cluster was created.
	slurmTimeout = 3 * time.Minute
)

// What TestMain found and wrote, the same for every test.
var (
	// binary is the clusterctl under test.
	binary string
	// knownHosts is the host key file the site trusts, a copy of sind's
	// that the host key tests change and restore.
	knownHosts string
	// sindKnownHosts is the host key file sind keeps for the realm, which
	// the copy is made from.
	sindKnownHosts string
	// environ is the environment clusterctl runs in.
	environ []string
)

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "clusterctl-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	// A missing cluster or sind fails the run rather than skipping it: a
	// suite that passes by running nothing would be read as a green one.
	if err := setUp(dir); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	return m.Run()
}

// setUp finds the binary and the cluster, and writes the configuration
// clusterctl reads into dir.
func setUp(dir string) error {
	var err error
	if binary, err = findBinary(dir); err != nil {
		return err
	}
	for _, tool := range []string{"sind", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf(`%s is not in PATH; the suite needs it, see doc/testing.md`, tool)
		}
	}
	if _, err := sind("get", "cluster", cluster); err != nil {
		return fmt.Errorf(`the sind cluster %s of the realm %s is not there; "make e2e-up" creates it: %w`, cluster, realm, err)
	}
	sshConfig, err := sindSSHConfig()
	if err != nil {
		return err
	}
	sindKnownHosts = filepath.Join(filepath.Dir(sshConfig), "known_hosts")
	if err := waitForSlurm(slurmTimeout); err != nil {
		return err
	}

	config := filepath.Join(dir, "config")
	if err := os.CopyFS(config, os.DirFS(filepath.Join("testdata", "site"))); err != nil {
		return err
	}
	knownHosts = filepath.Join(config, "ssh-known-hosts")
	if err := copyFile(sindKnownHosts, knownHosts); err != nil {
		return err
	}
	if err := writeWorkstation(filepath.Join(config, "workstation.yaml"), sshConfig); err != nil {
		return err
	}

	// Nothing of the caller's clusterctl settings reaches the binary, nor
	// its ssh agent, whose keys ssh would offer before sind's and which
	// could use up the attempts sshd allows. The state and the cache are
	// the suite's own, so the generated ssh configuration and the group
	// cache start empty.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "CLUSTERCTL_") && !strings.HasPrefix(v, "SSH_AUTH_SOCK=") &&
			!strings.HasPrefix(v, "XDG_STATE_HOME=") && !strings.HasPrefix(v, "XDG_CACHE_HOME=") {
			environ = append(environ, v)
		}
	}
	environ = append(environ,
		"CLUSTERCTL_CONFIG="+config,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
	)
	return nil
}

// findBinary returns the clusterctl to test: the one CLUSTERCTL_E2E_BINARY
// names, which is how CI tests the binary it built, or one built here.
func findBinary(dir string) (string, error) {
	if path := os.Getenv("CLUSTERCTL_E2E_BINARY"); path != "" {
		// go test runs the suite in its package directory, so a relative
		// path would not mean what it meant to the caller.
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("CLUSTERCTL_E2E_BINARY is %q; give an absolute path", path)
		}
		return path, nil
	}
	path := filepath.Join(dir, "clusterctl")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, "github.com/GSI-HPC/clusterctl/cmd/clusterctl")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building clusterctl: %w\n%s", err, out)
	}
	return path, nil
}

// sindSSHConfig returns the ssh configuration sind exports for the realm.
// clusterctl leaves out an include that does not exist, so its absence is
// found here rather than as a host name ssh cannot resolve.
func sindSSHConfig() (string, error) {
	out, err := sind("get", "ssh-config", "--output", "json")
	if err != nil {
		return "", err
	}
	var v struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("sind get ssh-config printed %q: %w", out, err)
	}
	if _, err := os.Stat(v.Path); err != nil {
		return "", fmt.Errorf("sind exports no ssh configuration for the realm %s: %w", realm, err)
	}
	return v.Path, nil
}

// waitForSlurm waits until slurmctld reports every worker idle.
func waitForSlurm(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := dockerExec("controller", "sinfo", "--noheader", "--Node", "--format", "%N %T")
		idle := []string{}
		for line := range strings.Lines(out) {
			if node, state, ok := strings.Cut(strings.TrimSpace(line), " "); ok && state == "idle" {
				idle = append(idle, node)
			}
		}
		if err == nil && containsAll(idle, workers) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("asking Slurm for the state of the workers: %w", err)
			}
			return fmt.Errorf("the workers were not idle in Slurm after %s:\n%s", timeout, out)
		}
		time.Sleep(2 * time.Second)
	}
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// writeWorkstation writes the workstation document, which holds what is
// true of this machine: where sind exported its ssh configuration. Only
// the system's configuration is included besides it, not the caller's own.
func writeWorkstation(path, sshConfig string) error {
	quoted, err := json.Marshal(sshConfig)
	if err != nil {
		return err
	}
	doc := fmt.Sprintf(`apiVersion: clusterctl/v1alpha1
kind: Workstation
spec:
  overrides:
    ssh.include:
      - %s
      - /etc/ssh/ssh_config
`, quoted)
	return os.WriteFile(path, []byte(doc), 0o644)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

// sind runs sind in the realm of the suite and returns what it printed.
func sind(args ...string) (string, error) {
	return output(exec.Command("sind", append([]string{"--realm", realm}, args...)...))
}

// container names the Docker container sind runs a node in.
func container(node string) string {
	return realm + "-" + cluster + "-" + node
}

// dockerExec runs a command in a node's container.
func dockerExec(node string, argv ...string) (string, error) {
	return output(exec.Command("docker", append([]string{"exec", container(node)}, argv...)...))
}

func output(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// onNode runs a command in a node's container with docker exec, and fails
// the test if it does not succeed. The tests check what clusterctl did this
// way, past both ssh and clusterctl.
func onNode(t *testing.T, node string, argv ...string) string {
	t.Helper()
	out, err := dockerExec(node, argv...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// result is what one run of clusterctl left behind.
type result struct {
	args   []string
	stdout string
	stderr string
	code   int
}

func (r result) String() string {
	return fmt.Sprintf("clusterctl %s exited %d\nstdout:\n%s\nstderr:\n%s",
		strings.Join(r.args, " "), r.code, r.stdout, r.stderr)
}

// wantCode fails the test unless clusterctl exited with the code given.
func (r result) wantCode(t *testing.T, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit code %d, want %d: %s", r.code, want, r)
	}
}

// clusterctl runs the binary under test in the environment of the suite,
// with an empty standard input, which is not a terminal.
func clusterctl(t *testing.T, args ...string) result {
	t.Helper()
	return clusterctlWithInput(t, "", args...)
}

// clusterctlWithInput runs clusterctl with the given standard input.
func clusterctlWithInput(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = environ
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{args: args, stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && ctx.Err() == nil:
		r.code = exit.ExitCode()
	default:
		t.Fatalf("clusterctl %s did not run to its end: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, r.stdout, r.stderr)
	}
	t.Logf("clusterctl %s: exit %d", strings.Join(args, " "), r.code)
	return r
}

// decode reads what clusterctl printed with -o json.
func decode[T any](t *testing.T, r result) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("the output is not the JSON expected: %v\n%s", err, r)
	}
	return v
}
