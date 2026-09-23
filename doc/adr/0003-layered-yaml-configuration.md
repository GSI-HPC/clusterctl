<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0003 — Layered YAML documents with provenance

Status: accepted

## Context

The toolkit carried about ninety flat environment variables, data in five
formats — shell, CSV, genders, ClusterShell group files, sshuttle argument
files — and one shell could not switch between clusters. Some variables were
dead, and some did not match the variable they were supposed to mirror.

## Decision

Versioned YAML documents of five kinds, merged in seven layers, with the
origin of every value kept and reportable.

## Why not the obvious alternatives

- **viper or koanf.** Both lose which layer set a value and where it was
  written. That is the single feature that makes a layered configuration usable
  instead of mystifying.
- **CUE as the core.** Its merge model expresses constraints, not override
  layers; "the workstation wins over the site" is not a CUE idea.
- **One flat file.** It does not serve several clusters, which is the job.

## How it works

Each document is decoded into a plain tree with positions, validated against
the JSON Schema generated from its Go type, and only then merged. Validating
before merging is what lets an error be reported at the line it was written on:

```
site.yaml:9:7: spec.hosts.login.forwardAgnet: unknown field "forwardAgnet"; did you mean "forwardAgent"?
```

Mappings merge key by key; sequences and scalars replace. A list of naming
rules merged element by element is a list nobody wrote.

## Costs

- Two validators: the schema for types, required fields and enums, and a walk
  of the same schema for unknown keys, because that is the error worth a
  suggestion and the generic message for it is unhelpful.
- The schema is generated from Go types, so a documentation string lives in a
  struct tag. That is the price of the schema and the code never disagreeing.
- `v1alpha1` promises nothing. When a field has to change, the version changes
  and a conversion is written; there is none yet because there is nothing to
  convert from.
