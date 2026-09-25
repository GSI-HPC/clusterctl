// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package exitcode defines the process exit codes clusterctl guarantees.
//
// The codes are part of the command line contract: scripts branch on them, so
// they may be added to but never renumbered.
package exitcode

import (
	"context"
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

// Has reports whether an error says which exit code it asks for.
func Has(err error) bool {
	var coded *Error
	return errors.As(err, &coded)
}

// Default gives an error the exit code code, unless it asks for one
// already.
func Default(code int, err error) error {
	if Has(err) {
		return err
	}
	return Wrap(code, err)
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

// Worst reports the code a command exits with when it met every one of
// errs, as on several hosts. The first that applies wins: Interrupted, which
// a cancellation counts as however it was wrapped, then Transport, Usage and
// TargetFailed. It is OK when every error is nil.
func Worst(errs ...error) int {
	code := OK
	for _, err := range errs {
		c := From(err)
		if errors.Is(err, context.Canceled) {
			c = Interrupted
		}
		if precedence(c) > precedence(code) {
			code = c
		}
	}
	return code
}

// precedence orders the codes for Worst: one that tells more about why the
// command did not succeed wins.
func precedence(code int) int {
	switch code {
	case OK:
		return 0
	case Usage:
		return 2
	case Transport:
		return 3
	case Interrupted:
		return 4
	default:
		return 1
	}
}
