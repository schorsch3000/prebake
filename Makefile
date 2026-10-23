export PATH := $(PATH):/usr/local/go/bin

.PHONY: build vet test lint
build:
	go build -o bin/prebake ./cmd/prebake
vet:
	go vet ./...
test:
	go test ./...
lint: vet
	@if ls scripts/*.sh >/dev/null 2>&1; then shellcheck scripts/*.sh; fi
