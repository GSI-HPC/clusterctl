// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package groups

import (
	"errors"
	"runtime"
	"testing"
)

// A lookup whose find panics hands the callers that waited for it an error
// of its own, not an empty answer taken for the group's nodes, lets the
// panic go on to its own caller, and keeps nothing: the next caller asks
// again.
func TestALookupThatPanicsIsForgotten(t *testing.T) {
	r := New(Options{})
	const key = "static\x00map\x00compute"
	started, release := make(chan struct{}), make(chan struct{})

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = r.lookup(key, func() (string, error) {
			close(started)
			<-release
			panic("the source's reply could not be read")
		})
	}()
	<-started
	waited := make(chan error, 1)
	go func() {
		_, err := r.lookup(key, func() (string, error) {
			t.Error("a caller that waited ran find itself")
			return "", nil
		})
		waited <- err
	}()
	// The panic comes once the second caller waits for the first.
	for {
		r.mu.Lock()
		waiting := r.flights[key].waiting
		r.mu.Unlock()
		if waiting == 1 {
			break
		}
		runtime.Gosched()
	}
	close(release)
	if p := <-panicked; p == nil {
		t.Error("the panic of find did not reach its caller")
	}
	if err := <-waited; !errors.Is(err, errAbandoned) {
		t.Errorf("the caller that waited got %v, want errAbandoned", err)
	}
	expr, err := r.lookup(key, func() (string, error) {
		return "exe[1-4]", nil
	})
	if err != nil || expr != "exe[1-4]" {
		t.Errorf("the next lookup = %q, %v; want find run again", expr, err)
	}
}
