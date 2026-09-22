---
title: clusterctl
layout: hextra-home
---

{{< hextra/hero-badge >}}
  <span>Free and open source</span>
  {{< icon name="arrow-circle-right" attributes="height=14" >}}
{{< /hextra/hero-badge >}}

<div class="hx-mt-6 hx-mb-6">
{{< hextra/hero-headline >}}
  Administer HPC clusters&nbsp;<br class="sm:hx-block hx-hidden" />from one binary
{{< /hextra/hero-headline >}}
</div>

<div class="hx-mb-12">
{{< hextra/hero-subtitle >}}
  Reach every host of a site, select nodes with ClusterShell node set syntax,&nbsp;<br class="sm:hx-block hx-hidden" />
  run commands on them in parallel, drive their service processors,&nbsp;<br class="sm:hx-block hx-hidden" />
  reinstall them and administer Slurm.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx-mb-6">
{{< hextra/hero-button text="Get started" link="docs/getting-started" >}}
</div>

<div class="hx-mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Node sets that behave"
    subtitle="ClusterShell syntax, groups from node attributes, the workload manager or a table, and set operations that mean what they say."
  >}}
  {{< hextra/feature-card
    title="Commands that arrive intact"
    subtitle="An argument vector is quoted once and reassembled by the remote shell. A glob is not expanded on your laptop, and an apostrophe does not break anything."
  >}}
  {{< hextra/feature-card
    title="Nothing destructive by surprise"
    subtitle="Every command that changes something previews it, asks first, refuses protected hosts, and understands --dry-run."
  >}}
  {{< hextra/feature-card
    title="One configuration, many clusters"
    subtitle="Layered YAML with a schema, validated where it was written, and a command that says which layer set each value."
  >}}
  {{< hextra/feature-card
    title="Secrets that stay put"
    subtitle="Passwords never reach a remote argument vector. age encrypted files are decrypted into memory and streamed to the node."
  >}}
  {{< hextra/feature-card
    title="One static binary"
    subtitle="No interpreter, no virtual environment, no agent on the nodes. Copy one file and run it."
  >}}
{{< /hextra/feature-grid >}}
