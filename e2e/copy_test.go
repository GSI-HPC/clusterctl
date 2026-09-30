// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyUploadsToEachNode copies a file to every worker with scp, through
// the same generated configuration as ssh, and reads it back on each node
// past clusterctl.
func TestCopyUploadsToEachNode(t *testing.T) {
	const remote = "/tmp/clusterctl-e2e-upload"
	content := "uploaded by the end-to-end tests\nwith a second line\n"
	local := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(local, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	// The mode is set again, since the umask may have taken from it.
	if err := os.Chmod(local, 0o640); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, node := range workers {
			_, _ = dockerExec(node, "rm", "-f", remote)
		}
	})

	r := clusterctl(t, "copy", "-y", "--preserve", "-n", "worker-[0-2]", local, remote)
	r.wantCode(t, 0)
	for _, node := range workers {
		if got := onNode(t, node, "cat", remote); got != content {
			t.Errorf("%s holds %q, want %q", node, got, content)
		}
		if got := strings.TrimSpace(onNode(t, node, "stat", "-c", "%a", remote)); got != "640" {
			t.Errorf("%s holds the file with mode %s, want 640, which --preserve keeps", node, got)
		}
	}
}

// TestCopyDownloadsIntoADirectoryPerNode collects a file that differs on
// every node, and checks that each node's copy lands under its own name.
func TestCopyDownloadsIntoADirectoryPerNode(t *testing.T) {
	const remote = "/tmp/clusterctl-e2e-download"
	for _, node := range workers {
		onNode(t, node, "sh", "-c", "uname -n > "+remote)
	}
	t.Cleanup(func() {
		for _, node := range workers {
			_, _ = dockerExec(node, "rm", "-f", remote)
		}
	})

	dir := t.TempDir()
	r := clusterctl(t, "copy", "-y", "--download", "-n", "worker-[0-2]", remote, dir+"/")
	r.wantCode(t, 0)
	for _, node := range workers {
		got, err := os.ReadFile(filepath.Join(dir, node, filepath.Base(remote)))
		if err != nil {
			t.Errorf("%s: %v", node, err)
			continue
		}
		if string(got) != node+"\n" {
			t.Errorf("the copy from %s holds %q, want its own name", node, got)
		}
	}
}
