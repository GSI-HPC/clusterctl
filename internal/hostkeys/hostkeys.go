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
var DefaultAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoRSASHA256,
}

// Entry is one line of a known_hosts file.
type Entry struct {
	// Hosts are the patterns the line applies to.
	Hosts []string `json:"hosts" yaml:"hosts"`
	// Type is the key algorithm, "ssh-ed25519" and the like.
	Type string `json:"type" yaml:"type"`
	// Key is the base64 encoded public key.
	Key string `json:"key" yaml:"key"`
	// Comment is whatever followed the key on the line.
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
}

// String renders the entry as a known_hosts line.
func (e Entry) String() string {
	line := strings.Join(e.Hosts, ",") + " " + e.Type + " " + e.Key
	if e.Comment != "" {
		line += " " + e.Comment
	}
	return line
}

// Matches reports whether the entry covers a host name.
func (e Entry) Matches(host string) bool {
	for _, h := range e.Hosts {
		if h == host {
			return true
		}
	}
	return false
}

// File is a parsed known_hosts file: the comment block at the top and the
// entries under it.
type File struct {
	// Header holds the comment lines at the top of the file, which sites use
	// to say what the file is and how it is maintained.
	Header []string
	// Entries are the host keys.
	Entries []Entry
}

// Parse reads a known_hosts file.
func Parse(data []byte) (*File, error) {
	f := &File{}
	body := false
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if !body {
				f.Header = append(f.Header, line)
			}
			continue
		}
		body = true
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("line %d is not a host key: %q", i+1, raw)
		}
		f.Entries = append(f.Entries, Entry{
			Hosts:   strings.Split(fields[0], ","),
			Type:    fields[1],
			Key:     fields[2],
			Comment: strings.Join(fields[3:], " "),
		})
	}
	return f, nil
}

// Render writes the file back, keeping the header on top and sorting the
// entries so that two refreshes produce the same file and a diff stays
// readable.
func (f *File) Render() []byte {
	var b strings.Builder
	for _, line := range f.Header {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	entries := append([]Entry(nil), f.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		a, c := strings.Join(entries[i].Hosts, ","), strings.Join(entries[j].Hosts, ",")
		if a != c {
			return a < c
		}
		return entries[i].Type < entries[j].Type
	})
	for _, e := range entries {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Find returns the entries that cover a host.
func (f *File) Find(host string) []Entry {
	var out []Entry
	for _, e := range f.Entries {
		if e.Matches(host) {
			out = append(out, e)
		}
	}
	return out
}

// Remove drops every entry covering a host and reports how many went.
func (f *File) Remove(host string) int {
	kept := f.Entries[:0]
	removed := 0
	for _, e := range f.Entries {
		if !e.Matches(host) {
			kept = append(kept, e)
			continue
		}
		if len(e.Hosts) == 1 {
			removed++
			continue
		}
		// A line covering several hosts keeps the others.
		var rest []string
		for _, h := range e.Hosts {
			if h != host {
				rest = append(rest, h)
			}
		}
		e.Hosts = rest
		kept = append(kept, e)
		removed++
	}
	f.Entries = kept
	return removed
}

// Add puts an entry in, replacing one of the same host and type.
func (f *File) Add(e Entry) {
	for i, existing := range f.Entries {
		if existing.Type == e.Type && len(existing.Hosts) == 1 && existing.Matches(e.Hosts[0]) {
			f.Entries[i] = e
			return
		}
	}
	f.Entries = append(f.Entries, e)
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
