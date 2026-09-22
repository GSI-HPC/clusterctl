// SPDX-License-Identifier: LGPL-3.0-or-later

package hostkeys_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
)

const sample = `# Host keys of the example site.
# Maintained with "clusterctl hostkey refresh".
exe0002.hpc.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB2 comment
exe0001.hpc.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB1
wlm01.hpc.example.org,wlm01 ssh-rsa AAAAB3NzaC1yc2EAAAAD
`

func TestParseAndRender(t *testing.T) {
	t.Parallel()

	f, err := hostkeys.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got, want := len(f.Header), 2; got != want {
		t.Errorf("header has %d lines, want %d", got, want)
	}
	if got, want := len(f.Entries), 3; got != want {
		t.Fatalf("got %d entries, want %d", got, want)
	}

	// Rendering sorts, so two refreshes produce the same file and a diff
	// stays readable.
	out := string(f.Render())
	if !strings.HasPrefix(out, "# Host keys of the example site.") {
		t.Errorf("the header did not stay on top:\n%s", out)
	}
	first := strings.Index(out, "exe0001")
	second := strings.Index(out, "exe0002")
	if first < 0 || second < 0 || first > second {
		t.Errorf("the entries were not sorted:\n%s", out)
	}

	// The rendered file parses back into the same thing.
	again, err := hostkeys.Parse(f.Render())
	if err != nil {
		t.Fatalf("re-parsing failed: %v", err)
	}
	if len(again.Entries) != len(f.Entries) {
		t.Errorf("round trip changed the entry count")
	}
}

func TestFindAndRemove(t *testing.T) {
	t.Parallel()

	f, err := hostkeys.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.Find("exe0001.hpc.example.org")); got != 1 {
		t.Errorf("Find returned %d entries, want 1", got)
	}
	if got := f.Remove("exe0001.hpc.example.org"); got != 1 {
		t.Errorf("Remove reported %d, want 1", got)
	}
	if got := len(f.Find("exe0001.hpc.example.org")); got != 0 {
		t.Errorf("the entry was not removed")
	}

	// A line covering several hosts keeps the others.
	if got := f.Remove("wlm01"); got != 1 {
		t.Errorf("Remove reported %d, want 1", got)
	}
	remaining := f.Find("wlm01.hpc.example.org")
	if len(remaining) != 1 {
		t.Fatalf("the shared line was dropped entirely")
	}
	if strings.Contains(strings.Join(remaining[0].Hosts, ","), "wlm01,") {
		t.Errorf("the removed host is still listed: %v", remaining[0].Hosts)
	}
}

func TestAddReplacesTheSameHostAndType(t *testing.T) {
	t.Parallel()

	f := &hostkeys.File{}
	f.Add(hostkeys.Entry{Hosts: []string{"exe1"}, Type: "ssh-ed25519", Key: "AAAA1"})
	f.Add(hostkeys.Entry{Hosts: []string{"exe1"}, Type: "ssh-ed25519", Key: "AAAA2"})
	f.Add(hostkeys.Entry{Hosts: []string{"exe1"}, Type: "ssh-rsa", Key: "AAAA3"})

	entries := f.Find("exe1")
	if got, want := len(entries), 2; got != want {
		t.Fatalf("got %d entries, want %d", got, want)
	}
	for _, e := range entries {
		if e.Type == "ssh-ed25519" && e.Key != "AAAA2" {
			t.Errorf("the key was not replaced: %q", e.Key)
		}
	}
}

func TestParseRejectsAMalformedLine(t *testing.T) {
	t.Parallel()

	if _, err := hostkeys.Parse([]byte("exe1 ssh-ed25519\n")); err == nil {
		t.Error("a line that is not a host key should be reported")
	}
}

func TestLoadAndSave(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "known_hosts")
	empty, err := hostkeys.Load(path)
	if err != nil {
		t.Fatalf("loading a missing file should succeed: %v", err)
	}
	if len(empty.Entries) != 0 {
		t.Error("a missing file should read as empty")
	}

	empty.Add(hostkeys.Entry{Hosts: []string{"exe1"}, Type: "ssh-ed25519", Key: "AAAA"})
	if err := hostkeys.Save(path, empty); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	loaded, err := hostkeys.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 1 {
		t.Errorf("the entry did not survive: %+v", loaded.Entries)
	}
}

// TestModifyIsAtomicUnderConcurrency is the property the shell tools did not
// have: ssh-keygen -R and an appending ssh-keyscan lose entries when two
// administrators refresh at the same moment.
func TestModifyIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "known_hosts")
	const writers = 8

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := hostkeys.Modify(context.Background(), path, func(f *hostkeys.File) error {
				f.Add(hostkeys.Entry{
					Hosts: []string{"exe" + string(rune('0'+i))},
					Type:  "ssh-ed25519",
					Key:   "AAAA",
				})
				return nil
			})
			if err != nil {
				t.Errorf("Modify failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	f, err := hostkeys.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.Entries); got != writers {
		t.Errorf("the file holds %d entries, want %d; a write was lost", got, writers)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestScanReportsAnUnreachableHost(t *testing.T) {
	t.Parallel()

	s := &hostkeys.Scanner{Port: "1", Timeout: 200_000_000}
	if _, err := s.Scan(context.Background(), "127.0.0.1"); err == nil {
		t.Error("scanning a closed port should be reported")
	}
}

func TestEntryString(t *testing.T) {
	t.Parallel()

	e := hostkeys.Entry{Hosts: []string{"a", "b"}, Type: "ssh-ed25519", Key: "AAAA", Comment: "note"}
	if got, want := e.String(), "a,b ssh-ed25519 AAAA note"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	if !e.Matches("b") || e.Matches("c") {
		t.Error("Matches does not agree with the host list")
	}
}
