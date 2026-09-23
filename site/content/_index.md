---
title: clusterctl
layout: hextra-home
# What link previews show. Without it they take the start of the page's
# text, which here runs into the labels of the node map.
description: >-
  One binary. Every node. No surprises. clusterctl selects nodes with
  ClusterShell syntax, fans out commands, drives service processors,
  reinstalls nodes and administers Slurm, and asks before it changes anything.
---

<div class="cx-home">
<section class="cx-band">
<div class="cx-band-in">
<div>
<p class="cx-kicker">clusterctl · for the people who run HPC clusters</p>
<h1>One binary.<br>Every node.<br><span>No surprises.</span></h1>
<p class="cx-lede">Select nodes with ClusterShell syntax, fan out commands, drive service processors, reinstall nodes and administer Slurm. Anything that changes something shows you first and asks.</p>
<div class="cx-actions">
<a class="cx-btn cx-btn-primary" href="docs/getting-started/">Get started <span aria-hidden="true">→</span></a>
<a class="cx-btn cx-btn-ghost" href="reference/">Commands</a>
</div>
<div class="cx-install"><span class="cx-prompt">$</span><code>mise use -g github:GSI-HPC/clusterctl</code></div>
</div>
<div>

{{< nodemap >}}

</div>
</div>
</section>

<section class="cx-facts">
<div><b>1</b><p>static binary. No interpreter, no virtual environment, no agent on the nodes.</p></div>
<div><b>0</b><p>passwords in an argument vector. Secrets are decrypted in memory and streamed.</p></div>
<div><b>7</b><p>configuration layers, one schema. <code>config explain</code> says which one won.</p></div>
<div><b class="cx-flag">--dry-run</b><p>on every command that changes something, and a question before it does.</p></div>
</section>

<section class="cx-commands">
<h2>Nouns and verbs, not scripts.</h2>
<p class="cx-sub">Every command is a noun then a verb, and the global flags mean the same thing in all of them.</p>
<div class="cx-command-grid">
<a href="docs/guides/node-sets/"><code>node select '@idle&amp;@rack:R02'</code><p>Node sets with groups from the inventory, the racks and Slurm, and set operations that mean what they say.</p></a>
<a href="docs/guides/running-commands/"><code>exec --dedup -- uname -r</code><p>A thousand nodes answer; the ones that agree collapse into one line. Your quoting arrives intact.</p></a>
<a href="docs/guides/power/"><code>bmc power on -n '@rack:R02'</code><p>Redfish first, pinned certificates, and a rack powered on in batches so the breaker stays in.</p></a>
<a href="docs/guides/reinstalling/"><code>provision reinstall --dry-run</code><p>PXE and GRUB boot paths, DHCP checks and secrets streamed to the node, rehearsed first.</p></a>
<a href="docs/guides/slurm/"><code>slurm node drain 'ticket 4711'</code><p>A drain needs a reason, so nobody has to guess before resuming. The queue, accounting and fair share too.</p></a>
<a href="docs/guides/agents/"><code>mcp serve</code><p>An AI agent reads the cluster and plans a change. It cannot apply one you have not confirmed.</p></a>
</div>
</section>

<section class="cx-close">
<div>
<h2>Asks before it breaks things.</h2>
<p>Above a threshold you type the number of hosts, because a <code>y</code> is easy to type by reflex. A protected host is refused, and so is powering off a node that runs a Slurm job, unless you say <code>--force</code>.</p>
<div class="cx-actions">
<a class="cx-btn cx-btn-primary" href="docs/getting-started/">Get started <span aria-hidden="true">→</span></a>
<a class="cx-btn cx-btn-ghost" href="https://github.com/GSI-HPC/clusterctl">GitHub</a>
</div>
</div>
<div>

{{< terminal >}}
$ clusterctl bmc power off -n '@rack:R02'
About to power off 10 hosts: exe[0001-0010]
  through Redfish
? This is more than 8 hosts. Type the number of hosts to continue: 10
{{< /terminal >}}

</div>
</section>
</div>
