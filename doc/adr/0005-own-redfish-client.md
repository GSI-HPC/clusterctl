<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0005 — Write the Redfish client rather than take gofish

Status: accepted

## Context

The review that preceded the rewrite recommends `stmcginnis/gofish` for
Redfish, having checked that it supports reset, reset-type discovery and boot
override. It also notes that gofish is pre-1.0, as are several of the libraries
it looked at.

The surface clusterctl needs is small: read the computer system, read the power
state, reset with a checked type, set and clear a boot source override, and
send an arbitrary GET or POST.

## Decision

Write the client, in `internal/redfish`. This is a deliberate deviation from
the review's dependency table.

## Why

Three requirements are about **how** a request is made, not what it asks for,
and each of them is easier to guarantee than to verify in a dependency:

- **The certificate is pinned.** A service processor carries a self signed
  certificate that no public authority vouches for. Verifying against the
  system roots is not an option, so the fingerprint seen first is recorded and
  a change is refused — what a known_hosts file does for host keys. The review
  asks for exactly this under "replace with a library: curl → native Redfish
  client with certificate pinning".
- **Nothing retries.** A reset that timed out may already have been carried
  out; sending it again power cycles a running machine. The review flags
  `go-retryablehttp`'s default policy for exactly this. A client with no retry
  logic at all cannot regress into having some.
- **TLS is configurable per vendor.** Old firmware needs a lower minimum
  version, and that has to reach the transport.

Beyond that: the JSON surface is shallow, the code is about 350 lines, and it
is covered by a fake service processor in the tests, including the cases that
matter — an unsupported reset type refused before it is sent, an action sent
exactly once, an override that defaults to applying once.

## Costs

- Vendor quirks are ours to discover. gofish has seen more firmware than this
  code will. The mitigation is `spec.bmc.vendors`, which lets a site pin reset
  types, the system path and TLS settings per vendor without a code change.
- Typed resource models are not there. Commands work with `map[string]any` for
  anything beyond the computer system, which is why `bmc redfish get` prints
  JSON rather than a table.

## Reconsider when

A vendor needs more of the schema than the computer system, or session-based
authentication becomes necessary. At that point gofish with a pinned
`http.Client` is the smaller change, and this record is superseded.
