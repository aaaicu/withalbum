BINARY ?= withalbum-server
.PHONY: test build fmt vet clean cross-build release-dryrun licenses

test:
	go test -race ./...

build:
	go build -trimpath -o $(BINARY) ./server

fmt:
	gofmt -w .

vet:
	go vet ./...

# Requires GoReleaser v2. Creates local release artifacts, not a GitHub release.
cross-build:
	goreleaser release --snapshot --clean

# Same archives and checksums without GoReleaser, outside the repository.
release-dryrun:
	bash scripts/release-dryrun.sh

# Refresh third_party/go-modules after a go.mod change.
licenses:
	bash scripts/third-party-licenses.sh

clean:
	rm -f $(BINARY)
	rm -rf dist
