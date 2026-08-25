#!/bin/sh
# Go toolchain agreement. The version lives in go.mod, but a container image
# tag cannot be read from a module file, so the builder image spells it a
# second time. Dependabot moves the Dockerfile on its own schedule and moves
# go.mod with it only when the module graph forces the issue, so the two
# drift silently — and a release whose binaries and container were built by
# different toolchains is not one the packaging can honestly call one build.
set -eu

mod=$(sed -n 's/^toolchain go\([0-9][0-9.]*\)$/\1/p' go.mod)
if [ -z "$mod" ]; then
  echo "no toolchain directive in go.mod" >&2
  exit 2
fi

# "FROM golang:1.27.0@sha256:... AS builder" -> "1.27.0"
img=$(sed -n 's|^FROM golang:\([0-9][0-9.]*\)[@ ].*|\1|p' Dockerfile)
if [ -z "$img" ]; then
  echo "no golang builder image in Dockerfile" >&2
  exit 2
fi

if [ "$mod" != "$img" ]; then
  echo "Go toolchain drift: go.mod says $mod, Dockerfile says $img" >&2
  echo "bump whichever is behind so the binaries and the container share a toolchain" >&2
  exit 1
fi

echo "Go toolchain agrees at $mod"
