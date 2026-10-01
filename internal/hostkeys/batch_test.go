// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package hostkeys_test

import (
	"crypto/hmac"
	"crypto/sha1" // ssh hashes known_hosts names with HMAC-SHA1
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
)

// The batch operations and the index were written to do in one pass what the
// operations on one host did one host after another. These are those, as
// they were, to compare with.

func names(pattern, host string) bool {
	if strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "*?") {
		return false
	}
	e := hostkeys.Entry{Hosts: []string{pattern}}
	return e.Matches(host)
}

func removeOne(f *hostkeys.File, host string) (int, []string) {
	kept := []hostkeys.Entry{}
	removed := 0
	var notes []string
	for _, e := range f.Entries {
		if !e.IsHostKey() {
			kept = append(kept, e)
			continue
		}
		var rest []string
		for _, h := range e.Hosts {
			if !names(h, host) {
				rest = append(rest, h)
			}
		}
		switch {
		case len(rest) == len(e.Hosts):
			kept = append(kept, e)
		case len(rest) == 0:
			notes = append(notes, e.Notes...)
			removed++
		default:
			e.Hosts = rest
			kept = append(kept, e)
			removed++
		}
	}
	f.Entries = kept
	return removed, notes
}

func replaceOne(f *hostkeys.File, host string, entries []hostkeys.Entry) {
	_, notes := removeOne(f, host)
	for i, e := range entries {
		if i == 0 && len(e.Notes) == 0 {
			e.Notes = notes
		}
		f.Add(e)
	}
}

func findOne(f *hostkeys.File, host string) []hostkeys.Entry {
	var out []hostkeys.Entry
	for _, e := range f.Entries {
		if e.IsHostKey() && e.Matches(host) {
			out = append(out, e)
		}
	}
	return out
}

func hashedName(host string) string {
	salt := []byte("salt-of-" + host)
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// knownHosts is a file with every kind of line a site's may hold: a host on
// a line of its own, several on one, a hashed name, wildcards, a negation,
// a revocation, a certificate authority, notes, and a name in capitals.
func knownHosts(t *testing.T) *hostkeys.File {
	t.Helper()
	data := "# The site's host keys.\n" +
		"exe1 ssh-ed25519 AAAA1\n" +
		"# exe2 was reinstalled on Monday\n" +
		"exe2,exe2.hpc ssh-ed25519 AAAA2\n" +
		"exe2 ecdsa-sha2-nistp256 EEEE2\n" +
		"# shared by the pair\n" +
		"exe3,exe4 ssh-ed25519 AAAA34\n" +
		hashedName("exe5") + " ssh-ed25519 AAAA5\n" +
		"exe*,!exe9 ssh-rsa RRRR\n" +
		"@revoked exe6 ssh-ed25519 BADKEY\n" +
		"@cert-authority *.hpc ssh-ed25519 CACA\n" +
		"EXE7 ssh-ed25519 AAAA7\n" +
		"exe8 ssh-ed25519 AAAA8 a comment\n" +
		"# trailer\n"
	f, err := hostkeys.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

var hostLists = [][]string{
	{"exe1"},
	{"exe2", "exe2.hpc"},
	{"exe3", "exe4"},
	{"exe4", "exe3"},
	{"exe5", "exe7", "exe9"},
	{"exe6", "exe8", "nope"},
	{"exe2", "EXE2", "exe2"},
	{"exe7", "exe1", "exe3", "exe5", "exe2", "exe4", "exe8", "exe2.hpc"},
	{},
}

func TestRemoveAllRemovesAsRemoveDoesOneAfterAnother(t *testing.T) {
	t.Parallel()

	for _, hosts := range hostLists {
		want := knownHosts(t)
		var counts []int
		for _, h := range hosts {
			n, _ := removeOne(want, h)
			counts = append(counts, n)
		}
		got := knownHosts(t)
		if n := got.RemoveAll(hosts); !slices.Equal(n, counts) {
			t.Errorf("RemoveAll(%v) counts %v, want %v", hosts, n, counts)
		}
		if g, w := string(got.Render()), string(want.Render()); g != w {
			t.Errorf("RemoveAll(%v) left\n%s\nwant\n%s", hosts, g, w)
		}
	}
}

func TestReplaceAllReplacesAsReplaceDoesOneAfterAnother(t *testing.T) {
	t.Parallel()

	keys := func(host string) []hostkeys.Entry {
		return []hostkeys.Entry{
			{Hosts: []string{host}, Type: "ssh-ed25519", Key: "NEW" + host},
			{Hosts: []string{host}, Type: "ecdsa-sha2-nistp256", Key: "NEWE" + host},
		}
	}
	for _, hosts := range hostLists {
		want := knownHosts(t)
		for _, h := range hosts {
			replaceOne(want, h, keys(h))
		}
		got := knownHosts(t)
		got.ReplaceAll(hosts, keys)
		if g, w := string(got.Render()), string(want.Render()); g != w {
			t.Errorf("ReplaceAll(%v) left\n%s\nwant\n%s", hosts, g, w)
		}
	}
}

func TestTheIndexFindsWhatTheFileFinds(t *testing.T) {
	t.Parallel()

	f := knownHosts(t)
	index := f.Index()
	for _, host := range []string{"exe1", "EXE1", "exe2", "exe2.hpc", "exe3", "exe4", "exe5", "exe6", "exe7",
		"exe8", "exe9", "exe10", "node.hpc", "nope", ""} {
		if got, want := index.Find(host), findOne(f, host); !slices.EqualFunc(got, want, func(a, b hostkeys.Entry) bool {
			return a.String() == b.String()
		}) {
			t.Errorf("Find(%q) = %v, want %v", host, got, want)
		}
		for _, key := range []string{"BADKEY", "AAAA1"} {
			e := hostkeys.Entry{Type: "ssh-ed25519", Key: key}
			if got, want := index.Revoked(host, e), f.Revoked(host, e); got != want {
				t.Errorf("Revoked(%q, %s) = %v, want %v", host, key, got, want)
			}
		}
	}
}

// Refreshing, removing or verifying the keys of a set read the whole file
// for each host, under the lock for the first two: half a minute for 10,000
// hosts, while every other writer of the file waited and gave up after 30
// seconds. Ten times as many hosts have to cost about ten times as much.
func TestHostKeyOperationsDoNotReadTheFileForEachHost(t *testing.T) {
	cost := func(n int) time.Duration {
		hosts := make([]string, n)
		var b strings.Builder
		for i := range hosts {
			hosts[i] = fmt.Sprintf("exe%05d.hpc", i)
			fmt.Fprintf(&b, "%s ssh-ed25519 AAAA%d\n", hosts[i], i)
		}
		keys := func(host string) []hostkeys.Entry {
			return []hostkeys.Entry{{Hosts: []string{host}, Type: "ssh-ed25519", Key: "NEW"}}
		}
		best := time.Duration(1 << 62)
		for range 3 {
			f, err := hostkeys.Parse([]byte(b.String()))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			index := f.Index()
			for _, h := range hosts {
				if len(index.Find(h)) != 1 {
					t.Fatalf("%s was not found", h)
				}
			}
			f.ReplaceAll(hosts, keys)
			f.RemoveAll(hosts)
			best = min(best, time.Since(start))
			if len(f.Entries) != 0 {
				t.Fatalf("%d entries are left", len(f.Entries))
			}
		}
		return best
	}
	small, large := cost(500), cost(5000)
	if large > 30*small {
		t.Errorf("5,000 hosts took %v, %d times what 500 took; it should be about 10", large, large/max(small, 1))
	}
}
