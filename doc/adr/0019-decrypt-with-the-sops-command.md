<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0019 — Decrypt Secret documents with the sops command

Status: accepted

It supersedes part of [0013](0013-sops-secret-documents.md): the decision "The
sops library, not the sops binary" and the costs "A larger binary" and "A
second dependency on encryption code".

## Context

[ADR 0013](0013-sops-secret-documents.md) linked the sops library so that no
workstation needs a matching sops installed. It accepted a binary growing from
11 MB to 46 MB, and looked no further at what the library brings. The footprint
study of issue #84 and the deduplication review of issue #90 have since
measured that, and issue #91 asked for the numbers again, at this change.

Reading one sops YAML file imports `sops/stores/yaml`, which imports
`sops/config`, which imports every key backend (AWS, Google Cloud, Azure,
Vault, Huawei) and the targets of `sops publish`; `keyservice.NewLocalClient`
imports them again. None of it can be left out while the library stays, and
with the default `sopsKeyTypes: [age]` none of it is called.

### What the library costs

Measured at `428f475`, the commit this change is based on, and with the change
applied. linux/amd64 unless said otherwise, `CGO_ENABLED=0`, the flags of
`.goreleaser.yaml` without its version stamps:

| | With the library | With the command |
| --- | --- | --- |
| Stripped release binary, linux/amd64 | 50.5 MB | 16.7 MB |
| The same, linux/arm64, darwin/amd64, darwin/arm64 | 47.1, 52.1, 49.0 MB | 15.3, 17.0, 15.8 MB |
| Packages linked, the standard library included | 1,105 | 329 |
| Packages linked from outside the standard library | 866 | 117 |
| Modules linked, clusterctl not counted | 135 | 28 |
| Lines of those modules' linked Go files | 1,205,172 | 159,799 |
| of which generated | 625,475 | 25,444 |
| Requirements in `go.mod` | 139 | 32 |
| Modules in the build graph, `go list -m all` | 363 | 52 |

The method, to reproduce them:

```sh
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -trimpath -ldflags '-s -w' -o clusterctl ./cmd/clusterctl && stat -c %s clusterctl
go list -deps ./cmd/clusterctl | wc -l
go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/clusterctl | grep -c .
go list -deps -f '{{if not .Standard}}{{with .Module}}{{.Path}}{{end}}{{end}}' ./cmd/clusterctl |
  sort -u | grep -v -e '^$' -e '^github.com/GSI-HPC/clusterctl$' | wc -l
```

The lines are those of every `.go` file in `GoFiles` of each linked package
outside the standard library and outside clusterctl, counted with `wc -l`; a
file is generated when its first 4 KiB say "Code generated" and "DO NOT
EDIT". Test files are not counted. The `go.mod` count is the lines of its two
`require` blocks.

107 of the 139 requirements in `go.mod` were there for sops alone, the figure
#84 found. With them go the AWS SDK and smithy, the envoy control plane, gRPC,
protobuf, xDS, CEL and genproto, the Google Cloud and Azure SDKs, MSAL, the
Vault and Huawei clients, OpenTelemetry, MongoDB BSON, `lib/pq`, a PGP
implementation and sops itself. `go list -deps ./cmd/clusterctl` now names no
sops, cloud SDK, gRPC, protobuf or OpenTelemetry package.

What #84 and #90 did not count, since the import in `77df581` on 22 September
2026:

- **Dependabot.** One pull request for a Go module, #12 (gRPC 1.83.1 to
  1.83.2). gRPC is required only by sops. The other eight were GitHub
  Actions.
- **govulncheck.** The first CI run of the pull request that brought sops in
  failed on GO-2026-6348, a memory exhaustion in the HTTP/2 transport of gRPC
  1.82.1, reported as reachable through package initialisation. `cfc9d14` took
  gRPC 1.83.1 for it. The same run reported two more vulnerabilities in
  imported packages and one in a required module that no code calls; the log
  does not name them.

Both changes to `go.mod` for a dependency's sake since the import concerned a
module only sops needs. So did the one vulnerability that failed CI.

### ADR 0013's arguments, checked again

- **"No workstation needs a matching sops installed."** clusterctl does not
  encrypt: creating, editing and re-keying a Secret is what `sops` does, so
  sops is already installed wherever a Secret is written. The command adds it
  wherever one is read.
- **"Every key type sops supports works."** It still does, and more closely:
  the sops that tries the keys is the one the site configured.
- **"A second implementation of a format to keep correct."** This counted
  against a native age reader, and still does. Running the command writes no
  implementation at all: sops stays the only reader of its format.

[ADR 0004](0004-drive-openssh.md) made the same choice for SSH: drive the
site's own client, so that its setup keeps working and `ssh -v` gives the same
answer. For sops that means `sops decrypt` succeeds or fails the way
clusterctl does, and `SOPS_AGE_KEY_FILE`, gpg-agent and KMS credentials are
used the way sops uses them.

## Decision

clusterctl runs the `sops` command to decrypt a Secret document, and links no
part of sops.

- **Reading without decrypting** stays in clusterctl. `InspectSops` reads the
  `sops` mapping with the YAML parser the loader already uses: the master keys
  of every type, named as sops names them, the key groups and the threshold,
  `lastmodified`, `mac`, `encrypted_regex` and `mac_only_encrypted`. Loading
  and `secrets check` need neither a key nor sops.
- **Decrypting** is `sops --config /dev/null decrypt --input-type yaml
  --output-type json --decryption-order age,pgp`, the file on standard input
  and the plaintext, as JSON, on standard output, into memory.
- **sops 3.10.0 or later**, the first that reads the file from standard input
  and opens it with an OpenSSH key. It is looked up in `PATH`, as the ssh
  client is, unless `workstation.sopsBinary` names another, as
  `sshuttleBinary` does for sshuttle. Its version is checked once per process,
  before it first decrypts.
- **`workstation.identities` are handed over by path.** sops opens an age
  identity file through `SOPS_AGE_KEY_FILE` and an OpenSSH key through
  `SOPS_AGE_SSH_PRIVATE_KEY_FILE`, one of each per run. clusterctl reads each
  file first, as it does for `ageFile` secrets, notes the recipients it opens,
  and runs sops once for each file that holds a recipient of the Secret, in
  the order configured. Then, at a terminal only, sops looks for a key itself.
- **The identities are read once, in app.** `secrets.IdentityFiles` reads a
  file into both forms, the identities it holds and its path, and `app` keeps
  the result for the process: `ageFile` and `source` secrets take the
  identities, sops the paths. Measure 19 of issue #90 changes the same code:
  the credential resolver still reads `workstation.identities` for itself, and
  the measure is to have it take them from `app`. That stays with the
  measure, which issue #91 leaves out of scope; this change only makes sure
  it adds no third reading of the files, and that `app` holds the one the
  resolver can be given.
- **The tests encrypt with sops**, pinned in `mise.toml` and CI, and run once
  more at the oldest sops supported. The library is not kept even for the
  tests: a test requirement is still a requirement in `go.mod`, and still one
  Dependabot proposes.

`filippo.io/age` stays: `ageFile` and `source` secrets use it.

## The risks, and what was done about each

| Risk | With the library | With the command | Mitigation |
| --- | --- | --- | --- |
| **sops missing or too old** | Cannot happen. | A Secret cannot be read. | `MinSopsVersion` is 3.10.0. The version is checked before the first decryption, and the error names the version needed. `doctor` checks it whenever the configuration holds a Secret. Nothing else runs sops: a test runs `config validate`, `node list` and `secrets check` with an empty `PATH`. |
| **Which binary runs** | The version in `go.mod`. | Whatever `sops` is first in `PATH`, possibly a different one on each workstation. | Looked up in `PATH`, as ssh is, or `workstation.sopsBinary` (`CLUSTERCTL_SOPS_BINARY`). `doctor` and the caption of `secrets check` show its path and version. |
| **Untrusted key types** | Refused before any key is tried. | sops tries every master key the metadata names, and the metadata is not covered by the MAC. | `CheckKeyTypes` stays in clusterctl, before sops runs. sops is given, on standard input, exactly the bytes that were checked, so the file cannot change in between. `InspectSops` also refuses a field of the `sops` mapping it does not know, which could be a kind of key a newer sops tries, and keys both inside and outside `key_groups`, where sops reads only the latter. A fake sops records whether it was started for such a file. |
| **Key discovery without a terminal** | Off: only `workstation.identities` are tried. | sops reads `SOPS_AGE_KEY*`, runs `SOPS_AGE_KEY_CMD` and `SOPS_AGE_SSH_PRIVATE_KEY_CMD`, reads `~/.config/sops/age/keys.txt`, `~/.ssh/id_ed25519` and `~/.ssh/id_rsa`, asks gpg-agent, and can prompt. | A run with `workstation.identities` gets an environment built from an allowlist, `PATH`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR` and `TZ`; `HOME`, `XDG_CONFIG_HOME` and `GNUPGHOME` point at an empty directory; and it runs in a session of its own, with no controlling terminal. sops runs at all only for an identity file that holds a recipient of the file. The rule is in `doc/mcp.md` and `doc/configuration.md`. What remains: when that identity fails to open its key, sops goes on to the file's other trusted key types, and a cloud SDK can still find the machine's own credentials, from instance metadata, without the environment. A site that trusts a KMS type on such a machine accepts that. |
| **Handing over `workstation.identities`** | In memory: any number, age and OpenSSH keys alike. | age keys through `SOPS_AGE_KEY`, in the environment, or `SOPS_AGE_KEY_FILE`, one file; OpenSSH keys through `SOPS_AGE_SSH_PRIVATE_KEY_FILE`, one per run. | sops opens the configured files by path, so no key is copied to the environment, a pipe or the disk. `SOPS_AGE_KEY` would put the key where the same user can read it in `/proc`, and cannot carry an OpenSSH key. An inherited pipe does not work: sops opens the key file again for each master key it tries, and a pipe is empty the second time, which was checked against sops 3.13.3 with a file whose second recipient was the one given. Several OpenSSH identities are handled with one run per file that holds a recipient, usually one. |
| **Standard streams** | Not involved. | Under `mcp serve`, clusterctl's standard input and output carry the protocol. | sops gets pipes of its own for all three. |
| **Plaintext on disk** | Never. | Never, as long as sops writes only to standard output. | Standard output is read into memory, bounded at 64 MiB. `--output` and `--in-place` are never passed, and there are no temporary files; a test records the arguments sops is run with. |
| **Values parsed again** | Read from the tree sops decrypted. | sops writes the plaintext and clusterctl parses it. | `--output-type json`, so no value is typed again. The JSON is read token by token: a value that is not a string, or a key given twice, is refused with its key named, and what the decoder says is never passed on. Multi-line values, tabs, U+2028, a trailing newline, an empty value and `binaryData` are checked against the real sops. |
| **Plaintext in error messages** | Every error after the data key opens is replaced by one fixed message. | sops' standard error can quote a decrypted value: a type tag changed to `int` makes sops 3.13.3 exit 25 with `strconv.Atoi: parsing "hunter2"`. | What sops prints is passed on only for exit 128, `CouldNotRetrieveKey`, when nothing was decrypted and it lists the keys it tried; folded to one line and escaped as untrusted text. Exits 51 and 52 are reported as changed without sops. Any other failure is reported by its exit status alone. |
| **Integrity** | clusterctl verifies the MAC. | sops verifies the MAC. | `--ignore-mac` is never passed. The `mac_only_encrypted` and type-tag checks stay in clusterctl, before sops runs. A changed name, a renamed key and a damaged MAC are put to the real binary. It refuses the name and the MAC with exit 51, and the renamed key with exit 25, since a value's path is part of what authenticates it; each is reported as changed without sops. |
| **Configuration read by sops** | None. | `sops decrypt` reads a `.sops.yaml` found from the working directory upwards, or `SOPS_CONFIG`. | `--config /dev/null` on every run, and `SOPS_CONFIG` is not passed on; a run with `workstation.identities` also runs in the empty directory. |
| **A hung or interrupted child** | Runs inside clusterctl. | A separate process. | Bound to the command's context. Cancelling sends SIGTERM, and `WaitDelay` gives up on it five seconds later, as for ssh. The version query has a timeout of its own; a decryption has none, since at a terminal sops may be waiting for a passphrase, as a credential helper may. |
| **Supply chain** | 107 modules in `go.mod`, and a clusterctl release for each of their fixes. | sops' own patch cycle, delivered by the distribution or a version manager ([ADR 0011](0011-installable-from-release-assets.md)). | The site updates sops on a workstation, as it updates ssh; mise's registry has it. CI and the release workflow build the version pinned as `SOPS_VERSION` from its tag with `go install`, which the Go checksum database vouches for; `mise.toml` pins the same. Dependabot updates neither, so `doc/release.md` lists them. |

Two more turned up while this was built:

- **`sops --version` asks GitHub** for the latest release unless
  `--disable-version-check` is given. It is given, and the variable that says
  the same is set.
- **sops' flags differ between versions.** 3.10.0 reads `--mac-only-encrypted`
  as a flag of `encrypt`, 3.13.3 as a flag of sops itself. The flags clusterctl
  decrypts with exist, with the same meaning, from 3.10.0 on; the job at the
  oldest version is what keeps that true.

## Alternatives

Measured the same way.

- **Keep the library.** No work, no requirement at run time, and clusterctl
  chooses the sops that decrypts. The first column of the table: 50.5 MB, 135
  modules, 107 requirements for sops alone, and every fix to one of them a
  clusterctl release.
- **A native age-only reader.** #90 measured the binary with the sops calls
  stubbed out at 16.3 MB, about what the command gives. It drops the PGP, KMS
  and Vault key types ADR 0013 promises, and it is the second implementation
  of the sops format ADR 0013 declined: the MAC over the whole tree, the
  path-bound additional data of every value, key groups and Shamir's scheme.
- **The hybrid: native age, the command for other key types.** The same
  binary, and both costs: the second implementation for age, sops at run time
  for everything else, and two decryption paths to test against each other.
- **The command with clusterctl as its key service**, through `--keyservice`,
  so that the identities never leave clusterctl. sops speaks gRPC to a key
  service, so clusterctl would serve sops' `KeyService` API. Importing sops'
  `keyservice` package for it brings the backends back, since its
  `server.go` imports every one of them: 26.2 MB, 870 packages, 109 modules,
  708,014 lines, 114 requirements. clusterctl's own copy of the code generated
  from `keyservice.proto` leaves the backends out but not gRPC and protobuf:
  at least 20.4 MB, 445 packages, 31 modules, 275,442 lines, 35 requirements,
  and a socket to protect, since sops dials a key service without
  authentication. What it would buy is small: the identities of
  `workstation.identities` are files already, which sops can open itself.

## What that costs

- **A requirement at run time.** A workstation that reads a Secret needs sops
  3.10.0 or later. One that reads none does not. The binary itself stays
  static, and requirement R69, no run-time dependencies of its own, holds as
  it does for ssh: sops is the site's program, which clusterctl runs, as
  [ADR 0004](0004-drive-openssh.md) has it run ssh. `doc/requirements.md`
  says so.
- **A version to check, and an interface to follow.** The command line of sops
  is now one clusterctl depends on: `decrypt` reading standard input, the flags
  above, `SOPS_AGE_KEY_FILE` and `SOPS_AGE_SSH_PRIVATE_KEY_FILE`, exit statuses
  128, 51 and 52. A sops release that changes them breaks decryption until
  clusterctl follows; `MinSopsVersion` rises when clusterctl needs something
  newer, with a line in the release notes.
- **clusterctl no longer chooses which sops decrypts.** A fix in sops or in
  one of its cloud SDKs arrives with the site's update of sops, not with a
  clusterctl release, and a workstation that never updates keeps its
  vulnerabilities. `doctor` shows which one is there.
- **Tests that need the binary.** `go test ./...` fails without sops in
  `PATH`; `mise install` installs it. CI builds it on every test job, and runs
  the suite a second time at the oldest version.
- **A process per decryption.** Each run of sops takes about 20 ms here, and
  the first decryption of a process runs it twice, once for the version. A
  Secret is decrypted once per process.
- **sops' words.** When no key opens a Secret, the error is sops' own list of
  the keys it tried, in its format, which can change between releases.
- **What sops does at a terminal is what sops does.** It finds keys, and can
  prompt, exactly as `sops decrypt` would at the same prompt, clusterctl's
  environment passed on except the variables that choose a key service, the
  configuration and the decryption order.

## What stays of 0013

The `Secret` kind, `secretRef`, the checks at load time, the trust in kinds of
key through `workstation.sopsKeyTypes`, decrypting into memory only when a
secret is used, and "clusterctl does not encrypt".
