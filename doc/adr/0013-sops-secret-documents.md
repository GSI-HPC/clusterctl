<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0013 — Secrets live in sops encrypted Secret documents

Status: superseded in part by [0019](0019-decrypt-with-the-sops-command.md)

## Context

A site keeps its configuration in version control, and a secret had to live
beside it as a file of its own: `ageFile` for a credential's password, `source`
for a file pushed onto the nodes. Every BMC password became a separate binary
file, and review of a change to one showed nothing but "binary files differ".

Many HPC teams already keep secrets with [sops](https://getsops.io): one YAML
file whose keys stay readable and whose values are encrypted, to age or PGP
keys or to a cloud key management service, with a message authentication code
over the whole file. Their `.sops.yaml`, their key rotation and their review
habits already exist; clusterctl should fit into them rather than add a second
way of encrypting.

## Decision

A new kind, `Secret`, holds values. It is a document of its own, alone in its
file, encrypted with sops so that only `data` and `binaryData` are encrypted:

```yaml
# secrets.sops.yaml, after: sops --encrypt --encrypted-regex '^(data|binaryData)$' --in-place secrets.sops.yaml
apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example
data:
  bmc-password: ENC[AES256_GCM,data:...,type:str]
binaryData:
  munge-key: ENC[AES256_GCM,data:...,type:str]
sops:
  age: [...]
  mac: ENC[...]
```

Other documents stay in plaintext and refer to a value by name and key:

```yaml
credentials:
  bmc:
    username: admin
    password:
      secretRef: {name: example, key: bmc-password}
services:
  cinc:
    secrets:
      - target: /etc/munge/munge.key
        secretRef: {name: example, key: munge-key}
```

- **References, not an encrypted configuration.** `secretRef` is one more
  password source, beside `fromEnv`, `file`, `ageFile`, `command` and `prompt`,
  and for a secret file it takes the place of `source`. Everything else in the
  configuration stays readable and reviewable. A document of any other kind
  that sops has encrypted is refused.
- **Nothing is decrypted while loading.** With only the values encrypted, the
  kind, the name and the keys of a Secret are readable without a key. The
  loader indexes Secrets by name, and every `secretRef` is checked against the
  keys when the configuration resolves, so a typo is reported at the line it
  was written on before any key is needed. A command that uses no secret runs
  on a machine that cannot read any.
- **Decrypted when used, into memory.** The credential resolver and
  `secrets push` decrypt the Secret document a reference names, once per
  process, with the sops library. The plaintext never touches the disk. The
  message authentication code is verified first, so a value or a name edited
  without sops is refused. The values are read from the tree sops decrypted,
  never written out and parsed again, and an error says nothing of them.
- **Keys come from the workstation, then from sops.** The age identities in
  `workstation.identities`, OpenSSH keys among them, are tried first, so a
  workstation that already opens `ageFile` secrets opens Secret documents with
  no more setup. Then, at a terminal only, sops looks itself:
  `SOPS_AGE_KEY_FILE` and its other variables, a PGP agent, or the
  credentials of a cloud KMS or Vault, age and PGP keys before any service,
  as the sops command orders them. Without a terminal its search, which can
  run a program or ask for a passphrase, is not used. When none works, the
  error names each key that was tried and why it failed.
- **Only the kinds of key the workstation trusts.** The keys a file is
  encrypted to are named in its sops metadata, which the message
  authentication code does not cover, and sops would contact whatever Vault
  or key management service is named there with this machine's credentials.
  A Secret naming a kind of key that `workstation.sopsKeyTypes` does not list,
  age alone by default, is refused before any key is tried.
- **Checked where it is written.** At load time the loader refuses a Secret
  that is not encrypted, a value that was added without sops, a Secret whose
  kind or name sops encrypted too (with the command that encrypts only the
  values), a Secret that is not alone in its file, a key given under both
  `data` and `binaryData`, and sops metadata it cannot read. It also refuses
  a value sops stored as anything but a string, which sops would parse after
  decrypting, by a type tag that is not authenticated, and a file encrypted
  with `mac_only_encrypted`, whose message authentication code leaves the
  kind and the name out.
- **`binaryData` for bytes.** A value under `data` is used as written, and a
  password loses a trailing newline. A value YAML would read as a number, a
  boolean or a date is quoted, or sops stores it as one. A value under `binaryData` is base64
  decoded first, because a munge key or a keytab is not text.
- **clusterctl does not encrypt.** Creating, editing and re-keying a Secret is
  what `sops` does, with the site's `.sops.yaml`. clusterctl ignores hidden
  files in a configuration directory, so `.sops.yaml` can sit next to the
  documents it applies to. `clusterctl secrets check` lists every Secret with
  its keys, the master keys it is encrypted to and how many references use
  it; `--decrypt` proves this workstation can open each one, in memory,
  printing nothing of the plaintext.
- **The sops library, not the sops binary.** The binary stays self-contained:
  no workstation needs a matching sops installed for clusterctl to read a
  Secret, and every key type sops supports works once the workstation trusts
  it.

## What that costs

- **A larger binary.** The sops library brings the AWS, Google Cloud and Azure
  SDKs, gRPC and a PGP implementation for its key management backends: the
  released binary grows from 11 MB to 46 MB. The alternative that
  avoids this, running the sops binary, moves a version requirement onto
  every workstation; a native implementation of the sops format for age alone
  would have been a second implementation of a format to keep correct.
- **A second dependency on encryption code.** age was already one. sops is
  well maintained and widely used, and Dependabot proposes its updates like
  any other requirement.
- **Encrypting only the values is required.** The default of `sops --encrypt`
  encrypts every value, the kind and the name included, and such a file is
  refused with the command that encrypts it correctly. The example
  `.sops.yaml` sets `encrypted_regex` so that the rule is written down once.
- **The names of the secrets are visible** to everyone who can read the
  repository, and so is when each file was last changed. That was already true
  of the `.age` files next to the documents.
- **One key failing is found when it is used.** Loading checks what can be
  checked without a key; whether this workstation holds one is only known by
  decrypting, which `secrets check --decrypt` does ahead of time.
