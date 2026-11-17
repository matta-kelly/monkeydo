set shell := ["bash", "-euo", "pipefail", "-c"]

asset := "dist/monkeydo-linux-amd64.tar.gz"

# List the commands
default:
    @just --list

# The check set, here and in CI: gofmt, go vet, go test
check:
    test -z "$(gofmt -l .)" || { gofmt -l .; echo "gofmt: these files are not formatted"; exit 1; }
    go vet ./...
    go test ./...

# The binary, with its version in it
build version="dev":
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o dist/monkeydo/bin/monkeydo ./cmd/monkeydo

# A release: check, build, tag v<version>, put the asset on the release, and print the pin dazoo takes
release version: check (build version)
    printf '%s\n' "{{version}}" > dist/monkeydo/VERSION
    tar -czf {{asset}} -C dist monkeydo
    git tag -a "v{{version}}" -m "monkeydo {{version}}" && git push origin "v{{version}}"
    gh release create "v{{version}}" {{asset}} --title "monkeydo {{version}}" --notes "monkeydo {{version}}"
    printf 'VERSION=%s\nSHA256=%s\n' "{{version}}" "$(sha256sum {{asset}} | cut -d' ' -f1)"
