export GOCACHE := $(CURDIR)/.build/cache
export GOMODCACHE := $(CURDIR)/.build/mod
GOFLAGS ?= -tags=with_utls
export GOFLAGS

.PHONY: build test race vet
build:
	go build -trimpath -o .build/veil ./cmd/veil

test:
	go test ./internal/...

race:
	go test -race ./internal/...

vet:
	go vet ./...

.PHONY: clean
clean:
	rm -rf -- .build
