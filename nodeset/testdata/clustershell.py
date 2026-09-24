#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later
"""Record ClusterShell's answer for every expression in clustershell.txt.

Reads the corpus, asks ClusterShell's NodeSet to expand each expression and
rewrites the second column with the answer: the host names, space separated,
or "error". The first and third columns are kept. Run it from this directory
with ClusterShell installed (pip install ClusterShell) after adding an
expression.
"""

import ClusterShell
from ClusterShell.NodeSet import NodeSet

PATH = "clustershell.txt"

out = []
with open(PATH) as f:
    for line in f:
        line = line.rstrip("\n")
        if not line or line.startswith("#"):
            if line.startswith("# Recorded with"):
                line = "# Recorded with ClusterShell " + ClusterShell.__version__ + "."
            out.append(line)
            continue
        cols = line.split("\t")
        expr = cols[0]
        tag = cols[2] if len(cols) > 2 else ""
        try:
            answer = " ".join(NodeSet(expr)) or "-"
        except Exception:
            answer = "error"
        out.append("\t".join([expr, answer, tag]).rstrip("\t"))

with open(PATH, "w") as f:
    f.write("\n".join(out) + "\n")
