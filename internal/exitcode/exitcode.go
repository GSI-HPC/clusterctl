// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package exitcode defines the process exit codes clusterctl guarantees.
//
// The codes are part of the command line contract: scripts branch on them, so
// they may be added to but never renumbered.
package exitcode

import (
	"errors"
	"fmt"
)

const (
	// OK means every target succeeded.
	OK = 0
	// TargetFailed means clusterctl ran correctly but at least one target
	// reported a failure.
	TargetFailed = 1
	// Usage means the command line or the configuration was rejected before
	// anything was contacted.
	Usage = 2
	// Transport means a host could not be reached, authenticated with, or
	// stayed reachable for the duration of the command.
	Transport = 3
	// Interrupted means the command was cancelled, by a signal or by a
	// declined confirmation prompt.
	Interrupted = 130
)

// Error carries an exit code alongside an error. Commands return it to select
// an exit code other than TargetFailed.
type Error struct {
	Code int
	Err  error
}

// Errorf builds an Error with a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Wrap attaches an exit code to an existing error, returning nil for nil.
func Wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// From reports the exit code an error should produce. Errors that carry no
// code of their own count as a target failure.
func From(err error) int {
	if err == nil {
		return OK
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return TargetFailed
}
