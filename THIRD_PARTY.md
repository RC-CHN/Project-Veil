# Third-party code

Veil's REALITY client adaptation in `veil-core/internal/transport/reality_client.go` derives from SagerNet/sing-box, commit `132b38e9caaba1a1959354d518e54d2d08419afe`, `common/tls/reality_client.go`, Copyright (C) 2022 nekohasekai. That code is GPL-3.0-or-later; this prototype is distributed under the same license (see LICENSE).

REALITY server, TLS and browser ClientHello support are provided by `github.com/metacubex/utls v1.8.7`. It includes upstream Go/uTLS/REALITY contributions with their licenses retained in the Go module and generated local copy. `veil-core/patches/` modifies that fixed source during builds without deleting notices. Go standard library overlays likewise retain the upstream BSD notice. Dependency versions and integrity sums are in veil-core/go.mod and veil-core/go.sum.

The `veil-service` component reuses the core and `golang.org/x/sys/unix` for Unix socket ownership locks. Its module requirements and integrity sums are retained in `veil-service/go.mod` and `veil-service/go.sum`.

OpenSSL is an optional system dependency (tested with 3.5.5, Apache-2.0). Go dependency source caches and generated code are excluded from version control; the build script recreates them.

The independent Rust interoperability peer uses the OpenSSL Rust bindings and the crates pinned in `interop/rust/Cargo.lock`. It does not link or import the Veil Go implementation. Cargo downloads dependency sources with their upstream licenses; they are not vendored into this repository.
