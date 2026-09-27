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

.PHONY: optimized demo clean
optimized:
	python3 scripts/build.py native
	python3 scripts/build.py batch
	python3 scripts/build.py openssl

demo: optimized
	python3 scripts/demo.py

clean:
	rm -rf -- .build
