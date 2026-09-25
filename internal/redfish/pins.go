// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// PinStore records the certificate a service processor presented, so that a
// change is noticed.
//
// Service processors carry self signed certificates that no public authority
// vouches for, so verifying against the system roots is not an option. What
// can be done is to remember the certificate seen the first time and refuse a
// silent change, which is what an ssh known_hosts file does for host keys.
type PinStore struct {
	// Path is the file the pins are kept in. A client refuses a store
	// without one, since it could pin nothing.
	Path string
}

// Fingerprint renders the SHA-256 fingerprint of a certificate.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Load reads the pins.
func (s *PinStore) Load() (map[string]string, error) {
	out := map[string]string{}
	if s.Path == "" {
		return out, nil
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, pin, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s line %d is not a pin: %q", s.Path, i+1, line)
		}
		out[host] = strings.TrimSpace(pin)
	}
	return out, nil
}

// Get returns the pin recorded for a host.
func (s *PinStore) Get(host string) (string, bool, error) {
	pins, err := s.Load()
	if err != nil {
		return "", false, err
	}
	pin, ok := pins[host]
	return pin, ok, nil
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
	return fileutil.Update(ctx, s.Path, 0o600, func(current []byte) ([]byte, error) {
		pins := map[string]string{}
		for line := range strings.SplitSeq(string(current), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if h, p, ok := strings.Cut(line, " "); ok {
				pins[h] = strings.TrimSpace(p)
			}
		}
		if recorded, ok := pins[host]; ok {
			if recorded != pin {
				return nil, &PinMismatchError{Host: host, Recorded: recorded, Seen: pin}
			}
			return current, nil
		}
		pins[host] = pin

		hosts := slices.Sorted(maps.Keys(pins))

		var b strings.Builder
		b.WriteString("# Certificate fingerprints of the service processors, recorded by clusterctl.\n")
		b.WriteString("# A changed fingerprint is refused; remove the line to accept a new certificate.\n")
		for _, h := range hosts {
			fmt.Fprintf(&b, "%s %s\n", h, pins[h])
		}
		return []byte(b.String()), nil
	})
}

// Remove drops the pin of a host, which is how a certificate replacement is
// accepted.
func (s *PinStore) Remove(ctx context.Context, host string) error {
	if s.Path == "" {
		return nil
	}
	return fileutil.Update(ctx, s.Path, 0o600, func(current []byte) ([]byte, error) {
		var b strings.Builder
		for line := range strings.SplitSeq(string(current), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if h, _, ok := strings.Cut(trimmed, " "); ok && h == host && !strings.HasPrefix(trimmed, "#") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	})
}

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
