// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that reads and writes as a string such as
// "30s". A bare number is rejected, because a configuration saying
// "timeout: 30" reads as nanoseconds and never means that.
type Duration time.Duration

// Get returns the value as a time.Duration.
func (d Duration) Get() time.Duration { return time.Duration(d) }

// Or returns the duration, or fallback when it is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"30s\": %w", err)
	}
	return d.parse(s)
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"30s\": %w", err)
	}
	return d.parse(s)
}

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: write it as \"30s\", \"5m\" or \"1h30m\"", s)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: it must not be negative", s)
	}
	*d = Duration(v)
	return nil
}

// JSONSchemaAlias tells the schema generator to describe a Duration as the
// string it is written as.
func (Duration) JSONSchemaAlias() any { return "" }
