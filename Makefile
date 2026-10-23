export PATH := $(PATH):/usr/local/go/bin

.PHONY: build vet test test-integration lint
build:
	go build -o bin/prebake ./cmd/prebake
vet:
	go vet ./...
test:
	go test ./...
# needs Docker and a registry on localhost:5001: docker run -d -p 5001:5000 registry:2
test-integration:
	go test -tags integration -count=1 -v ./internal/build/
lint: vet
	@if ls scripts/*.sh >/dev/null 2>&1; then shellcheck scripts/*.sh; fi
