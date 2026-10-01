// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// writePins writes a pin file of n processors.
func writePins(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "bmc%05d sha256:%064x\n", i, i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every handshake asks for a pin, and each parsed the whole file: a command
// over 10,000 processors spent minutes on it. A store reads the file once,
// and again only when it is no longer the file read, whoever changed it.
func TestPinStoreReadsTheFileOnceUntilItChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins")
	writePins(t, path, 4000)
	store := &redfish.PinStore{Path: path}
	if _, ok, err := store.Get("bmc00001"); err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	// Parsing the file again allocates for each of its 4,000 lines.
	if allocs := testing.AllocsPerRun(10, func() { _, _, _ = store.Get("bmc00002") }); allocs > 50 {
		t.Errorf("Get allocates %.0f times on a file of 4,000 pins that did not change", allocs)
	}

	// Another process records a pin, and then forgets one.
	other := &redfish.PinStore{Path: path}
	if err := other.Set(context.Background(), "new", "sha256:1111"); err != nil {
		t.Fatal(err)
	}
	if pin, ok, err := store.Get("new"); err != nil || !ok || pin != "sha256:1111" {
		t.Errorf("Get(new) = %q, %v, %v after another store recorded it", pin, ok, err)
	}
	if err := other.Remove(context.Background(), "bmc00001"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get("bmc00001"); err != nil || ok {
		t.Errorf("Get(bmc00001) = %v, %v after another store forgot it", ok, err)
	}
}

// A first contact appends its pin rather than rewriting the file, so that
// recording the pins of n processors costs in proportion to n: the file was
// read, sorted and written again for each of them.
func TestPinStoreRecordsFirstContactsInLinearTime(t *testing.T) {
	allocated := func(n int) uint64 {
		store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := range n {
			if err := store.Set(context.Background(), fmt.Sprintf("bmc%05d", i), fmt.Sprintf("sha256:%064x", i)); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		if pins, err := store.Load(); err != nil || len(pins) != n {
			t.Fatalf("%d pins recorded, want %d: %v", len(pins), n, err)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := allocated(40), allocated(400)
	if large > 20*small {
		t.Errorf("recording 400 pins allocated %d KiB, %d times what 40 cost; it should be about 10",
			large>>10, large/small)
	}
}

// A last line that has not ended is an append under way to a reader, which
// leaves it for the next read; to a first contact, which holds the lock so
// that no append can be under way, it is a line written by hand, which is a
// pin like any other and which the next pin does not run on into.
func TestPinStoreReadsALastLineWithoutItsEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(path, []byte("bmc1 sha256:aaaa\nbmc2 sha256:bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &redfish.PinStore{Path: path}
	if _, ok, err := store.Get("bmc2"); err != nil || ok {
		t.Errorf("Get(bmc2) = %v, %v; a line that has not ended is not read yet", ok, err)
	}
	var mismatch *redfish.PinMismatchError
	if err := store.Set(context.Background(), "bmc2", "sha256:cccc"); !errors.As(err, &mismatch) {
		t.Errorf("Set(bmc2) = %v, want the pin written by hand kept", err)
	}
	if err := store.Set(context.Background(), "bmc3", "sha256:dddd"); err != nil {
		t.Fatal(err)
	}
	pins, err := (&redfish.PinStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"bmc1": "sha256:aaaa", "bmc2": "sha256:bbbb", "bmc3": "sha256:dddd"} {
		if pins[host] != want {
			t.Errorf("%s is pinned to %q, want %q", host, pins[host], want)
		}
	}
}

// Overlapping first contacts in several processes, each with a store of its
// own, as a man in the middle could arrange against several commands at
// once: only one certificate may be accepted.
func TestPinStoreFirstContactsRaceAcrossStores(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pins")
	const contacts = 8
	errs := make([]error, contacts)
	var wg sync.WaitGroup
	for i := range contacts {
		wg.Go(func() {
			store := &redfish.PinStore{Path: path}
			errs[i] = store.Set(context.Background(), "bmc1", fmt.Sprintf("sha256:%04d", i))
		})
	}
	wg.Wait()

	pins, err := (&redfish.PinStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for i, err := range errs {
		var mismatch *redfish.PinMismatchError
		switch {
		case err == nil:
			accepted++
			if want := fmt.Sprintf("sha256:%04d", i); pins["bmc1"] != want {
				t.Errorf("contact %d was accepted, but %q is recorded", i, pins["bmc1"])
			}
		case errors.As(err, &mismatch):
		default:
			t.Errorf("contact %d: %v", i, err)
		}
	}
	if accepted != 1 {
		t.Errorf("%d different certificates were accepted at first contact, want 1", accepted)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "bmc1 "); n != 1 {
		t.Errorf("bmc1 is in the file %d times, want once:\n%s", n, data)
	}
}

// RemoveAll forgets every pin it is given and keeps every other line.
func TestPinStoreRemoveAll(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pins")
	store := &redfish.PinStore{Path: path}
	for _, host := range []string{"bmc1", "bmc2", "bmc3"} {
		if err := store.Set(context.Background(), host, "sha256:"+host); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RemoveAll(context.Background(), []string{"bmc1", "bmc3"}); err != nil {
		t.Fatal(err)
	}
	pins, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins["bmc2"] != "sha256:bmc2" {
		t.Errorf("pins = %v, want bmc2 alone", pins)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# Certificate fingerprints") {
		t.Errorf("the file lost its header:\n%s", data)
	}
}
