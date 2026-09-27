export GOCACHE := $(CURDIR)/.build/cache
export GOMODCACHE := $(CURDIR)/.build/mod
GOFLAGS ?= -tags=with_utls
export GOFLAGS
NATIVE_FLAGS = -modfile=$(CURDIR)/.build/native/build.mod

.PHONY: prepare build test race vet
build: prepare
	go build $(NATIVE_FLAGS) -trimpath -o .build/veil ./cmd/veil

test: prepare
	go test $(NATIVE_FLAGS) ./...

race: prepare
	go test $(NATIVE_FLAGS) -race ./...

vet: prepare
	go vet $(NATIVE_FLAGS) ./...

prepare:
	python3 scripts/build.py native --generate-only

.PHONY: optimized demo clean
optimized:
	python3 scripts/build.py native
	python3 scripts/build.py batch
	python3 scripts/build.py openssl

demo: optimized
	python3 scripts/demo.py

clean:
	rm -rf -- .build
