#!/usr/bin/env python3
"""Version-checked TLS overlays. Source templates are reviewable and retained.
Does not edit GOROOT, module cache, or upstream sources. Re-run after go.mod edits.
"""

import argparse
import hashlib
import json
import os
import pathlib
import shutil
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
ENV = os.environ | {
    "GOCACHE": str(ROOT / ".build/cache"),
    "GOMODCACHE": str(ROOT / ".build/mod"),
}


def run(args, **kw):
    return subprocess.run(args, cwd=ROOT, env=ENV, check=True, **kw)


def replace(s, a, b):
    if s.count(a) != 1:
        raise RuntimeError("upstream source changed: " + a[:80])
    return s.replace(a, b)


def conn_patch(s):
    padding_guard = " && c.out.veilPadRecords == 0" if "veilPadRecords" in s else ""
    s = replace(
        s,
        "\toutBufPtr := outBufPool.Get().(*[]byte)",
        """\tif typ == recordTypeApplicationData && c.vers == VersionTLS13 && !c.buffering && c.bytesSent >= recordSizeBoostThreshold && len(data)>maxPlaintextPADDING_GUARD {
        return c.veilWriteApplicationBatchLocked(data)
    }
\toutBufPtr := outBufPool.Get().(*[]byte)""".replace("PADDING_GUARD", padding_guard),
    )
    s = replace(
        s,
        "\thc.cipher = suite.aead(key, iv)",
        "\tveilReleaseAEAD(hc.cipher)\n\thc.cipher = suite.aead(key, iv)",
    )
    s = replace(
        s,
        "\tif x != 0 {\n\t\t// io.Writer and io.Closer",
        "\tdefer c.veilReleaseCrypto()\n\tif x != 0 {\n\t\t// io.Writer and io.Closer",
    )
    return s + (ROOT / "patches/batch.go.in").read_text()


def reader_patch(s):
    s = replace(
        s,
        "\tneeds := n - c.rawInput.Len()",
        "\tif c.veilDraining { return errVeilNeedRecord }\n\tneeds := n - c.rawInput.Len()",
    )
    s = replace(
        s,
        "\tc.rawInput.Grow(needs + bytes.MinRead)",
        """\tif c.isHandshakeComplete.Load() && c.vers==VersionTLS13 {
        const limit=veilBatchBytes
        // Compact explicitly so bytes.Buffer.Grow cannot double the cap.
        pending:=c.rawInput.Bytes()
        if c.rawInput.Cap()!=limit { c.rawInput=*bytes.NewBuffer(veilTakeBuffer()) } else { c.rawInput.Reset() }
        c.rawInput.Write(pending)
        r=io.LimitReader(r,int64(limit-c.rawInput.Len()))
    } else { c.rawInput.Grow(needs+bytes.MinRead) }""",
    )
    # ReadFrom may grow after its final read; atLeastReader returns EOF on that
    # read, preventing the extra Grow. Input limit bounds readable ciphertext.
    s = replace(
        s, "\trawInput ", "\tveilDraining bool // protected by in.Mutex\n\trawInput "
    )
    for call in ("recordHeaderLen", "recordHeaderLen+n"):
        a = "if err := c.readFromUntil(c.conn, " + call + "); err != nil {"
        s = replace(s, a, a + "\n\t\tif err == errVeilNeedRecord { return err }")
    s = read_patch(s)
    return s + (ROOT / "patches/reader.go.in").read_text()


def read_patch(s):
    # Conn and UConn have separate Read implementations and ticket handlers.
    s = replace(
        s,
        """\tfor c.input.Len() == 0 {
\t\tif err := c.readRecord(); err != nil {
\t\t\treturn 0, err
\t\t}""",
        """\tfor c.input.Len() == 0 {
        if c.hand.Len()==0 { if err:=c.readRecord();err!=nil{return 0,err} }""",
    )
    s = replace(
        s,
        "\tn, _ := c.input.Read(b)",
        """\tn, _ := c.input.Read(b)
    if c.vers==VersionTLS13 && len(b)>maxPlaintext {
        var err error;n,err=c.veilDrainBufferedRecords(b,n,c.handlePostHandshakeMessage);if err!=nil{return n,err}
    }""",
    )
    return s


def padding_patch(s):
    s = replace(
        s,
        "type halfConn struct {",
        "type halfConn struct {\n\tveilPadRecords, veilPadWindow uint8\n\tveilPadBytes, veilPadLimit uint16 // protected by Conn.out mutex",
    )
    return replace(
        s,
        "\t\t\trecord[0] = byte(recordTypeApplicationData)",
        """\t\t\tif recordType(record[0]) == recordTypeApplicationData {
                extra := hc.veilRecordPadding(len(payload))
                record = append(record, make([]byte, extra)...)
                padding += extra
            }
\t\t\trecord[0] = byte(recordTypeApplicationData)""",
    )


def reality_patch(s):
    # Keep the upstream entry point unchanged for differential probes. Veil's
    # hook owns only the forwarding pumps, not authentication or TLS parsing.
    signature = "func RealityServer(ctx context.Context, conn net.Conn, config *RealityConfig) (*Conn, error) {"
    s = replace(
        s,
        signature,
        signature
        + """
    return RealityServerWithFallback(ctx, conn, config, nil)
}

// RealityServerWithFallback permits a caller to own fallback forwarding.
// forward runs concurrently, once per direction. first contains any bytes
// already read from src, and is non-nil only for the downstream direction.
// It must finish forwarding before returning. Nil preserves upstream behavior.
func RealityServerWithFallback(ctx context.Context, conn net.Conn, config *RealityConfig, forward func(dst, src net.Conn, first []byte)) (*Conn, error) {
    copyFallback := func(dst, src net.Conn, first []byte, limit *RealityLimitFallback) {
        src = newRateLimitedConn(src, limit)
        if forward != nil {
            forward(dst, src, first)
            return
        }
        if first != nil { dst.Write(first) }
        io.Copy(dst, src)
    }
""",
    )
    upload = (
        "io.Copy(target, newRateLimitedConn(underlying, &config.LimitFallbackUpload))"
    )
    if s.count(upload) != 2:
        raise RuntimeError("upstream REALITY upload paths changed")
    s = s.replace(
        upload, "copyFallback(target, underlying, nil, &config.LimitFallbackUpload)"
    )
    return replace(
        s,
        "conn.Write(s2cSaved)\n\t\t\tio.Copy(underlying, newRateLimitedConn(target, &config.LimitFallbackDownload))",
        "copyFallback(underlying, target, s2cSaved, &config.LimitFallbackDownload)",
    )


def generate(mode):
    out = ROOT / ".build" / mode
    out.mkdir(parents=True, exist_ok=True)
    module = ROOT / ".build/mod/github.com/metacubex/utls@v1.8.7"
    if not module.exists():
        run(["go", "mod", "download"])
    reality = module / "reality.go"
    if (
        hashlib.sha256(reality.read_bytes()).hexdigest()
        != "9258864cc512e05302f5eb7d6a95efd7ae64bafcb45cb3b725086368740ab393"
    ):
        raise RuntimeError("unsupported source version: " + str(reality))
    patched_reality = out / "reality.go"
    patched_reality.write_text(reality_patch(reality.read_text()))
    run(["gofmt", "-w", str(patched_reality)])
    fork = out / "utls"
    if not fork.exists():
        shutil.copytree(module, fork)
    # Only our local module copy is made writable.
    for path in [fork, *fork.rglob("*")]:
        if not path.is_symlink():
            path.chmod(path.stat().st_mode | 0o200)
    shutil.copyfile(patched_reality, fork / "reality.go")
    conn = module / "conn.go"
    if (
        hashlib.sha256(conn.read_bytes()).hexdigest()
        != "e8027769d42706f263ef78d76ebd6e8a592299407a1f86b88b9ba5217bfe7dba"
    ):
        raise RuntimeError("unsupported source version: " + str(conn))
    patched_conn = reader_patch(padding_patch(conn.read_text()))
    if mode != "native":
        patched_conn = conn_patch(patched_conn)
    (fork / "conn.go").write_text(patched_conn)
    uconn = module / "u_conn.go"
    if (
        hashlib.sha256(uconn.read_bytes()).hexdigest()
        != "4a34d3717dfc4558980a7f344cb73982656ca3eba2acefb1cb35dd884a073467"
    ):
        raise RuntimeError("unsupported source version: " + str(uconn))
    (fork / "u_conn.go").write_text(read_patch(uconn.read_text()))
    shutil.copyfile(ROOT / "patches/padding.go.in", fork / "veil_padding.go")
    shutil.copyfile(ROOT / "patches/padding_test.go.in", fork / "veil_padding_test.go")
    run(
        [
            "gofmt",
            "-w",
            str(fork / "conn.go"),
            str(fork / "u_conn.go"),
            str(fork / "veil_padding.go"),
            str(fork / "veil_padding_test.go"),
        ]
    )
    mod = (ROOT / "go.mod").read_text()
    mod += f"\nreplace github.com/metacubex/utls => {fork}\n"
    (out / "build.mod").write_text(mod)
    shutil.copyfile(ROOT / "go.sum", out / "build.sum")
    if mode == "native":
        flags = ["-tags=with_utls", "-modfile=" + str(out / "build.mod")]
        (out / "flags.json").write_text(json.dumps(flags))
        return flags
    goroot = pathlib.Path(
        subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
    )
    checks = {
        module
        / "conn.go": "e8027769d42706f263ef78d76ebd6e8a592299407a1f86b88b9ba5217bfe7dba",
        module
        / "cipher_suites.go": "615e81c42d76df677c785f2552ebc07e8bf5fe83c07c35cd77c04e86c8aa8d3d",
        goroot
        / "src/crypto/tls/conn.go": "7e8523d78063ee21bed78a2fc7e8c29baaa3c156a4a25c6e0f9e8da4c7e83e51",
    }
    for path, digest in checks.items():
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise RuntimeError("unsupported source version: " + str(path))
    std = out / "std-conn.go"
    std.write_text(
        conn_patch(reader_patch((goroot / "src/crypto/tls/conn.go").read_text()))
    )
    suites = (module / "cipher_suites.go").read_text()
    if mode == "openssl":
        a = suites.index("func aeadAESGCMTLS13(")
        b = suites.index("\nfunc aeadChaCha20Poly1305", a)
        suites = (
            suites[:a]
            + """func aeadAESGCMTLS13(key,nonceMask []byte) aead {
 if len(nonceMask)!=aeadNonceLength {panic("tls: nonce mask length")}
 ret:=&xorNonceAEAD{aead:veilNewGCM(key)};copy(ret.nonceMask[:],nonceMask);return ret
}
"""
            + suites[b:]
        )
        (fork / "veil_openssl.go").write_text(
            (ROOT / "patches/openssl.go.in").read_text()
        )
    (fork / "cipher_suites.go").write_text(suites)
    tests = ROOT / "patches/record_test.go.in"
    if tests.exists():
        (fork / "veil_record_test.go").write_text(tests.read_text())
    shutil.copyfile(ROOT / "patches/uconn_test.go.in", fork / "veil_uconn_test.go")
    crypto_tests = ROOT / "patches/openssl_test.go.in"
    if mode == "openssl" and crypto_tests.exists():
        (fork / "veil_openssl_test.go").write_text(crypto_tests.read_text())
    run(
        ["gofmt", "-w", str(std), str(fork / "conn.go"), str(fork / "cipher_suites.go")]
        + [str(p) for p in fork.glob("veil*.go")]
    )
    overlay = {"Replace": {str(goroot / "src/crypto/tls/conn.go"): str(std)}}
    if tests.exists():
        overlay["Replace"][str(goroot / "src/crypto/tls/veil_record_test.go")] = str(
            tests
        )
    (out / "overlay.json").write_text(json.dumps(overlay, indent=2) + "\n")
    flags = [
        "-tags=with_utls" + (",veil_openssl" if mode == "openssl" else ""),
        "-overlay=" + str(out / "overlay.json"),
        "-modfile=" + str(out / "build.mod"),
    ]
    (out / "flags.json").write_text(json.dumps(flags))
    return flags


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("mode", choices=["native", "batch", "openssl"])
    p.add_argument("--generate-only", action="store_true")
    p.add_argument("--package", default="./cmd/veil")
    p.add_argument("--output")
    a = p.parse_args()
    flags = generate(a.mode)
    if not a.generate_only:
        run(
            [
                "go",
                "build",
                *flags,
                "-trimpath",
                "-o",
                a.output or str(ROOT / ".build" / ("veil-" + a.mode)),
                a.package,
            ]
        )
