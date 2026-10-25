#!/bin/sh
# Pinned compiler/plugins; generated Go files are committed, so normal builds
# do not need these tools or the original desktop bundle.
set -eu
cd "$(dirname "$0")/.."
tools_dir=$(mktemp -d)
trap 'rm -rf "$tools_dir"' EXIT HUP INT TERM
GOBIN="$tools_dir" go install github.com/bufbuild/buf/cmd/buf@v1.50.0
GOBIN="$tools_dir" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
GOBIN="$tools_dir" go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.19.1
PATH="$tools_dir:$PATH" buf generate
