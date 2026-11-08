#!/usr/bin/env python3
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
"""Fail when a leaf key of a chart's values.yaml has no helm-docs description.

helm-docs lists a key only when the comment block above it holds a `# --` line,
so an undocumented value silently drops out of the README's Values reference.
A leaf is a key with an inline value (`port: 80`, `labels: {}`, `list: []`).
It is covered when its own block has a `# --` line, or when an ancestor does
(`resources:` documented as one object). Keys that only open a nested map need
no description of their own.

usage: chart-values-coverage.py charts/<chart>/values.yaml ...
"""
import re
import sys

KEY = re.compile(r"^(\s*)([A-Za-z0-9_\-]+):(\s.*|)$")
DOC = re.compile(r"^\s*# -- ")


def missing(path):
    lines = open(path, encoding="utf-8").read().split("\n")
    stack = []  # (indent, key, documented)
    out = []
    for i, line in enumerate(lines):
        m = KEY.match(line)
        if not m:
            continue
        indent, key, rest = len(m.group(1)), m.group(2), m.group(3).strip()
        while stack and stack[-1][0] >= indent:
            stack.pop()
        j, documented = i - 1, False
        while j >= 0 and lines[j].lstrip().startswith("#"):
            documented = documented or bool(DOC.match(lines[j]))
            j -= 1
        stack.append((indent, key, documented))
        covered = any(d for _, _, d in stack)
        if rest and not rest.startswith("#") and not covered:
            out.append((i + 1, ".".join(k for _, k, _ in stack)))
    return out


def main(paths):
    bad = 0
    for p in paths:
        for line, key in missing(p):
            print(f"{p}:{line}: {key} has no '# --' description (helm-docs)")
            bad += 1
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
