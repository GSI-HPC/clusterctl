// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package hostkeys reads, writes and collects SSH host keys.
//
// The host key file is the site's trust anchor: it is kept under version
// control and every connection is checked against it. Editing it with
// ssh-keygen -R and appending with ssh-keyscan, as the shell tools did, is
// neither atomic nor locked, so two administrators refreshing at once could
// lose an entry. Everything here goes through one locked, atomic update.
package hostkeys

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // ssh hashes known_hosts names with HMAC-SHA1
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

// DefaultAlgorithms are the host key algorithms collected for a host, best
// first. Ed25519 is preferred; RSA is kept for service processors and older
// systems that offer nothing else.
//
// ssh-rsa comes last. It names the same RSA key as rsa-sha2-256 and differs
// only in the SHA-1 signature over the handshake, which matters for a session
// but not for reading the key. A server older than OpenSSH 7.2 signs with
// nothing else, and those are the hosts legacyAlgorithms exists for.
var DefaultAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSA,
}

// Markers a known_hosts line may start with.
const (
	// MarkerRevoked says the key on the line must never be accepted.
	MarkerRevoked = "@revoked"
	// MarkerCertAuthority says the key on the line signs host certificates;
	// it is not the key of a host.
	MarkerCertAuthority = "@cert-authority"
)

// Entry is one line of a known_hosts file.
type Entry struct {
	// Marker is "@revoked", "@cert-authority" or empty for a plain host key.
	Marker string `json:"marker,omitempty" yaml:"marker,omitempty"`
	// Hosts are the patterns the line applies to: names, wildcards,
	// negations and hashed names.
	Hosts []string `json:"hosts" yaml:"hosts"`
	// Type is the key algorithm, "ssh-ed25519" and the like.
	Type string `json:"type" yaml:"type"`
	// Key is the base64 encoded public key.
	Key string `json:"key" yaml:"key"`
	// Comment is whatever followed the key on the line.
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	// Notes are the comment lines written above the entry. They travel with
	// it when the file is sorted, so that a note stays with the key it is
	// about.
	Notes []string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// String renders the entry as a known_hosts line.
func (e Entry) String() string {
	line := strings.Join(e.Hosts, ",") + " " + e.Type + " " + e.Key
	if e.Marker != "" {
		line = e.Marker + " " + line
	}
	if e.Comment != "" {
		line += " " + e.Comment
	}
	return line
}

// IsHostKey reports whether the line is a plain host key, rather than a
// revocation or a certificate authority.
func (e Entry) IsHostKey() bool { return e.Marker == "" }

// Matches reports whether the entry covers a host name, the way ssh decides
// it: a hashed name is compared by its hash, * and ? are wildcards, and a
// negated pattern that matches rules the line out whatever else matches.
func (e Entry) Matches(host string) bool {
	host = strings.ToLower(host)
	hit := false
	for _, pattern := range e.Hosts {
		negated := strings.HasPrefix(pattern, "!")
		if !matchPattern(strings.TrimPrefix(pattern, "!"), host) {
			continue
		}
		if negated {
			return false
		}
		hit = true
	}
	return hit
}

// names reports whether a pattern names exactly this host: literally or by
// its hash, but not through a wildcard. Only such a pattern is removed or
// replaced for the host, because a wildcard also speaks for other hosts.
func names(pattern, host string) bool {
	if strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "*?") {
		return false
	}
	return matchPattern(pattern, strings.ToLower(host))
}

func matchPattern(pattern, host string) bool {
	if strings.HasPrefix(pattern, hashPrefix) {
		return matchHashed(pattern, host)
	}
	return matchWildcard(strings.ToLower(pattern), host)
}

// hashPrefix starts a name ssh-keygen -H hashed.
const hashPrefix = "|1|"

// matchHashed compares a host with a hashed name, |1|salt|hash, where hash
// is the HMAC-SHA1 of the host name keyed with the salt.
func matchHashed(pattern, host string) bool {
	salt64, hash64, ok := strings.Cut(strings.TrimPrefix(pattern, hashPrefix), "|")
	if !ok {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(hash64)
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), want)
}

// matchWildcard is ssh's pattern match: * is any run of characters and ? is
// one character. Nothing else is special.
func matchWildcard(pattern, s string) bool {
	for pattern != "" {
		switch pattern[0] {
		case '*':
			pattern = strings.TrimLeft(pattern, "*")
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchWildcard(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return s == ""
}

// File is a parsed known_hosts file: the comment block at the top, the
// entries under it and any comment after the last entry.
type File struct {
	// Header holds the comment lines at the top of the file, which sites use
	// to say what the file is and how it is maintained.
	Header []string
	// Entries are the host keys, revocations and certificate authorities.
	// A comment between entries is kept as a note of the entry under it.
	Entries []Entry
	// Trailer holds the comment lines after the last entry.
	Trailer []string
}

// Parse reads a known_hosts file.
func Parse(data []byte) (*File, error) {
	f := &File{}
	body := false
	var notes []string
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if body {
				notes = append(notes, line)
			} else {
				f.Header = append(f.Header, line)
			}
			continue
		}
		body = true
		fields := strings.Fields(line)
		var marker string
		if strings.HasPrefix(fields[0], "@") {
			marker = fields[0]
			if marker != MarkerRevoked && marker != MarkerCertAuthority {
				return nil, fmt.Errorf("line %d has an unknown marker %q", i+1, marker)
			}
			fields = fields[1:]
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("line %d is not a host key: %q", i+1, raw)
		}
		f.Entries = append(f.Entries, Entry{
			Marker:  marker,
			Hosts:   strings.Split(fields[0], ","),
			Type:    fields[1],
			Key:     fields[2],
			Comment: strings.Join(fields[3:], " "),
			Notes:   notes,
		})
		notes = nil
	}
	f.Trailer = notes
	return f, nil
}

// Render writes the file back, keeping the header on top and sorting the
// entries so that two refreshes produce the same file and a diff stays
// readable. Each entry is written under its notes.
func (f *File) Render() []byte {
	var b strings.Builder
	for _, line := range f.Header {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	entries := append([]Entry(nil), f.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		a, c := strings.Join(entries[i].Hosts, ","), strings.Join(entries[j].Hosts, ",")
		if a != c {
			return a < c
		}
		if entries[i].Marker != entries[j].Marker {
			return entries[i].Marker < entries[j].Marker
		}
		return entries[i].Type < entries[j].Type
	})
	for _, e := range entries {
		for _, note := range e.Notes {
			b.WriteString(note)
			b.WriteByte('\n')
		}
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	for _, line := range f.Trailer {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Find returns the host keys that cover a host. Revocations and certificate
// authorities are not host keys and are left out.
func (f *File) Find(host string) []Entry {
	var out []Entry
	for _, e := range f.Entries {
		if e.IsHostKey() && e.Matches(host) {
			out = append(out, e)
		}
	}
	return out
}

// Revoked reports whether the file revokes a key for a host.
func (f *File) Revoked(host string, key Entry) bool {
	for _, e := range f.Entries {
		if e.Marker == MarkerRevoked && e.Key == key.Key && e.Matches(host) {
			return true
		}
	}
	return false
}

// Remove drops the host keys naming a host and reports how many went.
//
// Only a pattern that names the host itself, literally or hashed, goes. A
// wildcard also covers other hosts, and a revocation or a certificate
// authority is a decision someone made on purpose, so neither is touched.
func (f *File) Remove(host string) int {
	n, _ := f.remove(host)
	return n
}

// remove drops the host keys naming a host and returns the notes of the lines
// that went entirely.
func (f *File) remove(host string) (int, []string) {
	kept := f.Entries[:0]
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
			// A line covering several hosts keeps the others.
			e.Hosts = rest
			kept = append(kept, e)
			removed++
		}
	}
	f.Entries = kept
	return removed, notes
}

// Add puts a host key in, replacing one of the same host and type and keeping
// its notes.
func (f *File) Add(e Entry) {
	for i, existing := range f.Entries {
		if existing.IsHostKey() && existing.Type == e.Type && len(existing.Hosts) == 1 &&
			len(e.Hosts) > 0 && names(existing.Hosts[0], e.Hosts[0]) {
			if len(e.Notes) == 0 {
				e.Notes = existing.Notes
			}
			f.Entries[i] = e
			return
		}
	}
	f.Entries = append(f.Entries, e)
}

// Replace puts a host's current keys in place of the ones the file holds for
// it, and carries the notes written above the old keys over to the new ones.
func (f *File) Replace(host string, entries []Entry) {
	_, notes := f.remove(host)
	for i, e := range entries {
		if i == 0 && len(e.Notes) == 0 {
			e.Notes = notes
		}
		f.Add(e)
	}
}

// Load reads a known_hosts file, treating a missing file as empty.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Save writes a known_hosts file atomically.
func Save(path string, f *File) error {
	return fileutil.WriteAtomic(path, f.Render(), 0o644)
}

// Modify reads, changes and writes a known_hosts file under a lock, so that
// two administrators refreshing at the same time cannot lose an entry.
func Modify(ctx context.Context, path string, change func(*File) error) error {
	return fileutil.Update(ctx, path, 0o644, func(current []byte) ([]byte, error) {
		f, err := Parse(current)
		if err != nil {
			return nil, err
		}
		if err := change(f); err != nil {
			return nil, err
		}
		return f.Render(), nil
	})
}

// errCollected stops the handshake once the host key has been seen. There is
// no reason to authenticate: the key is all that is wanted.
var errCollected = errors.New("host key collected")

// Scanner collects host keys by starting an SSH handshake and stopping as
// soon as the server has presented its key.
type Scanner struct {
	// Algorithms are the key types to ask for, best first.
	Algorithms []string
	// Timeout bounds one connection attempt.
	Timeout time.Duration
	// Port is the SSH port; empty means 22.
	Port string
	// Dial connects; it is replaced in tests.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Scan collects the host keys a host offers, best algorithm first. It returns
// the first key it obtains, which is the strongest algorithm the host
// supports out of those asked for.
func (s *Scanner) Scan(ctx context.Context, host string) ([]Entry, error) {
	algorithms := s.Algorithms
	if len(algorithms) == 0 {
		algorithms = DefaultAlgorithms
	}
	port := s.Port
	if port == "" {
		port = "22"
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dial := s.Dial
	if dial == nil {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, address)
		}
	}

	address := net.JoinHostPort(host, port)
	var lastErr error
	for _, algorithm := range algorithms {
		entry, err := scanOne(ctx, dial, address, host, algorithm, timeout)
		if err == nil {
			return []Entry{entry}, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no host key could be collected from %s: %w", host, lastErr)
}

func scanOne(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error),
	address, host, algorithm string, timeout time.Duration) (Entry, error) {

	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	var collected ssh.PublicKey
	config := &ssh.ClientConfig{
		// The handshake is abandoned before authentication, so the account
		// only has to be syntactically valid.
		User:              "clusterctl-hostkey-scan",
		HostKeyAlgorithms: []string{algorithm},
		Timeout:           timeout,
		// The key is captured and the handshake abandoned at once: the
		// key is the only thing wanted, and authenticating would need
		// credentials this command has no business holding.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			collected = key
			return errCollected
		},
	}

	_, _, _, err = ssh.NewClientConn(conn, address, config)
	if collected == nil {
		if err == nil {
			err = errors.New("the host presented no key")
		}
		return Entry{}, err
	}
	return Entry{
		Hosts: []string{host},
		Type:  collected.Type(),
		Key:   strings.TrimPrefix(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(collected))), collected.Type()+" "),
	}, nil
}
