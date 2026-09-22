<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0013 — Secrets encrypted with age, inline in the documents

Status: accepted

## Context

A site keeps its configuration in version control, and until now a secret had
to live beside it: `ageFile` for a credential's password, `source` for a file
pushed onto the nodes. Every BMC password became a file of its own, a password
change became a change to a binary file next to the document that names it,
and review of such a change showed nothing but "binary files differ".

The alternatives an administrator reaches for are worse. A password written in
the clear is not a supported source at all, and `fromEnv` or `command` move the
secret out of version control, so the next administrator does not have it.

age is already the site's encryption: `workstation.identities` names the keys,
an OpenSSH key is accepted as one, and `clusterctl secrets push` decrypts into
memory and streams the plaintext to the node.

## Decision

A secret may be written inline, as the ASCII armored age ciphertext, in a field
named `age`:

```yaml
spec:
  secrets:
    recipients:
      - age1...          # admin1
      - ssh-ed25519 ...  # admin2
      - age1...          # recovery key, kept offline
  credentials:
    bmc:
      username: admin
      password:
        age: |
          -----BEGIN AGE ENCRYPTED FILE-----
          ...
          -----END AGE ENCRYPTED FILE-----
  services:
    cinc:
      secrets:
        - target: /etc/slurm/jwt_hs256.key
          mode: "0600"
          age: |
            -----BEGIN AGE ENCRYPTED FILE-----
            ...
```

- **Only two places read one:** `credentials.*.password.age`, as a password
  source beside `fromEnv`, `file`, `ageFile`, `command` and `prompt`, exactly
  one of which is set; and `services.cinc.secrets[].age`, instead of `source`,
  exactly one of which is set.
- **Ciphertext anywhere else is an error.** A value that starts with the age
  armor header under any other key is reported by the validator. Otherwise it
  would be used as the literal text it is, and a BMC would be sent a page of
  base64 as its password.
- **Decrypted when used, not when loaded.** Loading the configuration needs no
  identity: `config view`, `node list` and the other commands that do not touch
  a secret keep working on a machine that cannot read any, and an encrypted SSH
  key is never asked for its passphrase by a command that does not need it.
  Decryption happens where the file based sources already decrypted, in the
  credential resolver and in `secrets push`, into memory.
- **Checked when loaded, without decrypting.** The validator reads the armor
  and the age header of every inline secret, so a truncated paste or a lost
  line is reported at the line it was written on, like any other mistake in a
  document. It checks that `secrets.recipients` holds public keys and never
  repeats an entry that is not one, since that entry may be a private key
  pasted by mistake.
- **The recipients are the site's.** `secrets.recipients` in the `Site`
  document lists who inline secrets are encrypted to: native age recipients,
  post-quantum hybrid ones and OpenSSH public keys. Plugin recipients are
  refused, because accepting one runs a program the configuration names.
- **The tool writes the ciphertext.** `clusterctl secrets encrypt` reads the
  value without echo, twice, or from standard input or `--file`, and prints the
  `age: |` block at the indentation it is pasted at. `clusterctl secrets check`
  lists every encrypted value, inline or file, with the kinds of recipient it
  names; `--decrypt` proves this workstation can read each one, in memory,
  printing nothing of the plaintext.
- **Output never carries the armor where a person reads it.**
  `config view --show-sources` and `config explain` print an inline secret as
  `<age encrypted, N bytes>`. The structured formats (`-o yaml`, `-o json`)
  print the ciphertext as it is written, so the output can be pasted back.

## What that costs

- **Re-encrypting is by hand.** When an administrator leaves, each inline
  secret is decrypted and encrypted again with `secrets encrypt` and pasted
  over the old one. Rewriting the documents in place would have to preserve
  comments, ordering and quoting, which the configuration loader does not
  model; a `secrets rekey` that does is future work. Rotating the secret itself
  is still needed, as with any file: the old ciphertext stays in the history.
- **The recipients of an X25519 stanza are not visible.** age does not name
  the recipient a stanza is for, so `secrets check` can count the recipients
  and name their kinds, but cannot say that a secret is missing the newest
  administrator. `secrets check --decrypt` run by that administrator can.
- **Structural checks do not cover the payload.** The header is checked when
  the configuration loads; a line lost from the middle of the payload is found
  only by decrypting, which `secrets check --decrypt` does.
- **Ciphertext makes a document longer.** A password is about ten lines of
  armor per recipient group. Large files, such as a keytab, still belong in a
  file of their own with `source`.
- **Everyone who can read the repository can see which secrets exist,** how
  large they are and how many recipients each has. That was already true of the
  `.age` files beside the documents.
