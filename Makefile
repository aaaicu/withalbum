BINARY ?= withalbum-server
.PHONY: test build fmt vet clean cross-build

test:
	go test -race ./...

build:
	go build -trimpath -o $(BINARY) ./server

fmt:
	gofmt -w server/*.go

vet:
	go vet ./...

# Requires GoReleaser v2. Creates local release artifacts, not a GitHub release.
cross-build:
	goreleaser release --snapshot --clean

clean:
	rm -f $(BINARY)
	rm -rf dist
