// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

// PinStore records the certificate a service processor presented, so that a
// change is noticed.
//
// Service processors carry self signed certificates that no public authority
// vouches for, so verifying against the system roots is not an option. What
// can be done is to remember the certificate seen the first time and refuse a
// silent change, which is what an ssh known_hosts file does for host keys.
//
// A store keeps the pins it read, and reads the file again only once it is
// no longer the file read: every TLS handshake asks for a pin, and parsing
// the whole file for each made a command over 10,000 processors spend
// minutes on it. A first contact appends its pin rather than rewriting the
// file, so a reader never needs the lock. One store is meant to serve every
// client of a command, and is safe for them to use at once.
type PinStore struct {
	// Path is the file the pins are kept in. A client refuses a store
	// without one, since it could pin nothing.
	Path string

	// mu orders the reads and the writes of this store, so that the
	// file's lock is only waited for across processes, and guards read.
	mu sync.Mutex
	// read is the file as it was last read, nil before it was.
	read *pinFile
}

// pinFile is a pin file as it was read.
type pinFile struct {
	pins map[string]string
	// info is the file that was read, and size how much of it was taken,
	// up to the end of its last line that had ended.
	info fs.FileInfo
	size int64
}

// pinHeader opens a new pin file.
const pinHeader = "# Certificate fingerprints of the service processors, recorded by clusterctl.\n" +
	"# A changed fingerprint is refused; remove the line to accept a new certificate.\n"

// Fingerprint renders the SHA-256 fingerprint of a certificate.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Load reads the pins.
func (s *PinStore) Load() (map[string]string, error) {
	if s.Path == "" {
		return map[string]string{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pins, err := s.current(s.Path, false)
	return maps.Clone(pins), err
}

// Get returns the pin recorded for a host.
func (s *PinStore) Get(host string) (string, bool, error) {
	if s.Path == "" {
		return "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pins, err := s.current(s.Path, false)
	if err != nil {
		return "", false, err
	}
	pin, ok := pins[host]
	return pin, ok, nil
}

// current returns the pins of the file at path, read again only when it is
// not the file last read. A last line that has not ended is an append under
// way, and is left for the next read, unless locked says the lock is held
// and no append can be: then it is a line written without its end, by hand.
// The caller holds s.mu.
func (s *PinStore) current(path string, locked bool) (map[string]string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.read = nil
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if r := s.read; r != nil && os.SameFile(r.info, info) && info.Size() == r.size && info.ModTime().Equal(r.info.ModTime()) {
		return r.pins, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.read = nil
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err = f.Stat(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if !locked {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	pins := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, pin, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s line %d is not a pin: %q", s.Path, i+1, line)
		}
		pins[host] = strings.TrimSpace(pin)
	}
	s.read = &pinFile{pins: pins, info: info, size: int64(len(data))}
	return pins, nil
}

// Set records the pin of a host that has none. It never replaces a pin: when
// a different one is recorded, including by a concurrent first contact that
// got there first, it returns a PinMismatchError. The check and the write
// happen under one lock, so of overlapping first contacts that present
// different certificates only one is accepted. Remove is how a pin is
// replaced on purpose.
func (s *PinStore) Set(ctx context.Context, host, pin string) error {
	if s.Path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fileutil.Locked(ctx, s.Path, func(path string) error {
		pins, err := s.current(path, true)
		if err != nil {
			return err
		}
		if recorded, ok := pins[host]; ok {
			if recorded != pin {
				return &PinMismatchError{Host: host, Recorded: recorded, Seen: pin}
			}
			return nil
		}
		line := host + " " + pin + "\n"
		switch {
		case s.read == nil:
			line = pinHeader + line
		case s.read.size > 0 && !s.endsLine(path):
			// A last line written by hand without its end would
			// otherwise run on into this one.
			line = "\n" + line
		}
		if err := fileutil.AppendSync(path, []byte(line), 0o600); err != nil {
			return err
		}
		// What was read and what was appended is the file now; it is
		// read again only if anyone else has written to it since, or
		// when it cannot be looked at. The map is only ever read under
		// s.mu, so it takes the pin in place.
		s.read = nil
		if info, err := os.Stat(path); err == nil {
			pins[host] = pin
			s.read = &pinFile{pins: pins, info: info, size: info.Size()}
		}
		return nil
	})
}

// endsLine reports whether the file at path ends with the end of a line.
func (s *PinStore) endsLine(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, s.read.size-1); err != nil {
		return true
	}
	return last[0] == '\n'
}

// Remove drops the pin of a host, which is how a certificate replacement is
// accepted.
func (s *PinStore) Remove(ctx context.Context, host string) error {
	return s.RemoveAll(ctx, []string{host})
}

// RemoveAll drops the pins of the hosts, with one rewrite of the file.
func (s *PinStore) RemoveAll(ctx context.Context, hosts []string) error {
	if s.Path == "" {
		return nil
	}
	drop := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		drop[h] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read = nil
	return fileutil.Update(ctx, s.Path, 0o600, func(current []byte) ([]byte, error) {
		var b strings.Builder
		for line := range strings.SplitSeq(string(current), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if h, _, ok := strings.Cut(trimmed, " "); ok && drop[h] && !strings.HasPrefix(trimmed, "#") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	})
}

// PinStoreError says that the pin store could not be read or written while a
// processor's certificate was checked. The handshake stopped there, so no
// request was sent.
type PinStoreError struct {
	Host string
	Err  error
}

func (e *PinStoreError) Error() string {
	return fmt.Sprintf("%s: the certificate could not be checked against the pin store: %v", e.Host, e.Err)
}

func (e *PinStoreError) Unwrap() error { return e.Err }

// PinMismatchError says that a service processor presented a different
// certificate than the one recorded.
type PinMismatchError struct {
	Host     string
	Recorded string
	Seen     string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf(
		"the certificate of %s changed: recorded %s, now %s. "+
			"If the certificate was replaced on purpose, run \"clusterctl bmc forget %s\" and try again",
		e.Host, e.Recorded, e.Seen, e.Host)
}

// ProgressClass says that the certificate did not match its pin.
func (e *PinMismatchError) ProgressClass() progress.Class { return progress.ClassPin }
