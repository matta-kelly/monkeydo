set shell := ["bash", "-euo", "pipefail", "-c"]

asset := "dist/monkeydo-linux-amd64.tar.gz"

# List the commands
default:
    @just --list

# The check set: scripts/check.sh, which crate's merge queue runs too
check:
    scripts/check.sh

# The binary, with its version in it
build version="dev":
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o dist/monkeydo/bin/monkeydo ./cmd/monkeydo

# The release's tarball, dist/monkeydo-linux-amd64.tar.gz, with the binary and its VERSION. publish.yml runs it on a version tag crate made
tarball version: (build version)
    printf '%s\n' "{{version}}" > dist/monkeydo/VERSION
    tar -czf {{asset}} -C dist monkeydo
