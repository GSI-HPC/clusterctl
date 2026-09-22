<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0004 — Drive OpenSSH rather than speak SSH

Status: accepted

## Context

Everything this program does to a node goes over SSH. Go has a capable SSH
library in `golang.org/x/crypto/ssh`, and using it would mean no external
process, no quoting and a typed error for every failure.

## Decision

Run the OpenSSH client as a subprocess, with a generated `-F` configuration.
Use `x/crypto/ssh` only to collect host keys, where no session is established.

## Why

An HPC site's SSH setup is not simple. `ProxyJump` chains, `ControlMaster`,
Kerberos and GSSAPI, agent forwarding, PKCS#11 tokens, per-host `IdentityFile`,
distribution crypto policies, FreeIPA host certificates — all of it is already
configured on the administrator's machine and all of it has to keep working.

Reimplementing that is not a feature, it is a second, worse SSH client that
differs from the one the team debugs with. When `clusterctl login` fails, the
next thing anyone types is `ssh -v`, and the answer has to be the same.

## Costs

- **Quoting.** A remote command has to survive a shell. This is the failure
  that cost the old toolkit the most, so it is handled in one place,
  `internal/shellquote`, and checked against a real shell in the tests.
- **Errors are exit codes and stderr.** ssh's 255 is mapped to the transport
  exit code and the last line of stderr is reported, which is less precise than
  a typed error.
- **A generated configuration file.** Its ordering matters — settings before
  includes, trust settings in the file rather than in `-o` because `-o` does
  not reach `ProxyJump` hops. All of this is documented in
  [../transport.md](../transport.md) because it is not obvious.

## Note on host key collection

Collecting a host key needs a handshake but not a session, so it uses
`x/crypto/ssh` with a callback that captures the key and abandons the
handshake. No credentials are involved, which is the point: the alternative is
`ssh-keyscan`, another process with its own defaults.
