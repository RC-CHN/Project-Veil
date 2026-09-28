//! Standalone protocol conformance peer. No Veil source/library dependency.
mod crypto;
mod reality;
mod server;
mod wire;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use crypto::*;
use openssl::ssl::{SslConnector, SslMethod, SslVersion};
use serde_json::{Value, json};
use std::{
    fs,
    io::{Read, Write},
    net::{Shutdown, SocketAddr, TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    thread,
    time::Duration,
};
use wire::*;
pub type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;

fn socket(address: SocketAddr) -> Result<TcpStream> {
    let s = TcpStream::connect_timeout(&address, Duration::from_secs(3))?;
    s.set_nodelay(true)?;
    s.set_read_timeout(Some(Duration::from_secs(5)))?;
    s.set_write_timeout(Some(Duration::from_secs(5)))?;
    Ok(s)
}
struct ChildGuard(Child);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
struct Folder(PathBuf);
impl Drop for Folder {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn spawn(cmd: &mut Command) -> Result<ChildGuard> {
    Ok(ChildGuard(
        cmd.stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()?,
    ))
}
fn wait_port(addr: SocketAddr, child: &mut ChildGuard) -> Result<()> {
    for _ in 0..300 {
        if let Some(status) = child.0.try_wait()? {
            return Err(format!("child exited {status}").into());
        }
        if TcpStream::connect_timeout(&addr, Duration::from_millis(100)).is_ok() {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(10));
    }
    Err("listener timeout".into())
}
fn vacant() -> Result<SocketAddr> {
    Ok(TcpListener::bind("127.0.0.1:0")?.local_addr()?)
}
fn echo_backend() -> Result<SocketAddr> {
    let l = TcpListener::bind("127.0.0.1:0")?;
    let addr = l.local_addr()?;
    thread::spawn(move || {
        for stream in l.incoming() {
            let Ok(mut stream) = stream else { break };
            thread::spawn(move || {
                let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
                let mut b = [0; 32768];
                loop {
                    match stream.read(&mut b) {
                        Ok(0) => {
                            let _ = stream.shutdown(Shutdown::Write);
                            break;
                        }
                        Ok(n) => {
                            if stream.write_all(&b[..n]).is_err() {
                                break;
                            }
                        }
                        Err(_) => break,
                    }
                }
            });
        }
    });
    Ok(addr)
}
fn connect(addr: SocketAddr, mode: &str, cert: &Path, rpub: &[u8], sid: &[u8]) -> Result<Channel> {
    let io = socket(addr)?;
    if mode == "reality" {
        return Ok(Channel::Reality(reality::Connection::connect(
            io,
            "cover.test",
            rpub,
            sid,
        )?));
    }
    let mut ctx = SslConnector::builder(SslMethod::tls_client())?;
    ctx.set_min_proto_version(Some(SslVersion::TLS1_3))?;
    ctx.set_max_proto_version(Some(SslVersion::TLS1_3))?;
    ctx.set_ca_file(cert)?;
    ctx.set_alpn_protos(b"\x08http/1.1")?;
    Ok(Channel::Tls(ctx.build().connect(
        "cover.test",
        Prefixed {
            saved: std::io::Cursor::new(Vec::new()),
            stream: io,
        },
    )?))
}
fn exercise(io: Channel, k: &[u8], target: SocketAddr, refused: SocketAddr) -> Result<usize> {
    let auth = proof(k, &io.exporter()?, &random(16)?)?;
    let mut peer = Peer::new(io);
    peer.open(1, target, &auth)?;
    peer.send(1, b"independent Rust client")?;
    peer.finish(1, b"independent Rust client")?;
    // >256 application frames requires real credit grants, independent of record splitting.
    peer.open(3, target, b"")?;
    let mut expected = Vec::new();
    for i in 0..600 {
        let p = vec![(i % 251) as u8; 257];
        peer.send(3, &p)?;
        expected.extend(p);
        peer.echo(3, &expected)?;
    }
    peer.finish(3, &expected)?;
    if peer.grants < 345 {
        return Err("flow-control path was not exercised".into());
    }
    peer.open(5, refused, b"")?;
    if peer.failed(5) != Some(3) {
        return Err("expected refused FAILED(3)".into());
    }
    for id in (7..23).step_by(2) {
        peer.open(id, target, b"")?;
    }
    for id in (7..23).step_by(2) {
        peer.send(id, &vec![id as u8; 4096])?;
    }
    for id in (7..23).step_by(2) {
        peer.finish(id, &vec![id as u8; 4096])?;
    }
    peer.open(23, target, b"")?;
    peer.reset(23)?;
    peer.open(25, target, b"")?;
    peer.send(25, b"after reset")?;
    peer.finish(25, b"after reset")?;
    Ok(peer.grants)
}
fn selftest(binary: &Path) -> Result<()> {
    let binary = fs::canonicalize(binary)?;
    let folder =
        Folder(std::env::temp_dir().join(format!("veil-rust-interop-{}", hex::encode(random(8)?))));
    fs::create_dir(&folder.0)?;
    let cert = folder.0.join("cert.pem");
    let private_file = folder.0.join("key.pem");
    let status = Command::new("openssl")
        .args([
            "req",
            "-x509",
            "-newkey",
            "ec",
            "-pkeyopt",
            "ec_paramgen_curve:P-256",
            "-nodes",
            "-days",
            "1",
            "-subj",
            "/CN=cover.test",
            "-addext",
            "subjectAltName=DNS:cover.test",
            "-keyout",
        ])
        .arg(&private_file)
        .arg("-out")
        .arg(&cert)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    if !status.success() {
        return Err("fixture certificate generation".into());
    }
    let cover = vacant()?;
    let mut cover_proc = spawn(
        Command::new("openssl")
            .args(["s_server", "-accept"])
            .arg(cover.to_string())
            .arg("-cert")
            .arg(&cert)
            .arg("-key")
            .arg(&private_file)
            .args([
                "-tls1_3",
                "-ciphersuites",
                "TLS_AES_128_GCM_SHA256",
                "-groups",
                "X25519",
                "-www",
                "-alpn",
                "http/1.1",
            ]),
    )?;
    wait_port(cover, &mut cover_proc)?;
    let target = echo_backend()?;
    let reserved = TcpListener::bind("127.0.0.1:0")?;
    let refused = reserved.local_addr()?;
    drop(reserved);
    let k = random(32)?;
    let private = random(32)?;
    let public = x_public(&private)?;
    let sid = random(8)?;
    for mode in ["tls", "reality"] {
        let address = vacant()?;
        let mut tls = json!({"mode":mode,"server_name":"cover.test","record_padding":true});
        if mode == "tls" {
            tls["certificate"] = json!(cert);
            tls["private_key_file"] = json!(private_file);
        } else {
            tls["reality_private_key"] = json!(URL_SAFE_NO_PAD.encode(&private));
            tls["short_id"] = json!(hex::encode(&sid));
            tls["cover_address"] = json!(cover.to_string());
        }
        let cfg = json!({"role":"server","listen":address.to_string(),"secret":URL_SAFE_NO_PAD.encode(&k),"tls":tls});
        let config = folder.0.join(format!("{mode}.json"));
        fs::write(&config, serde_json::to_vec(&cfg)?)?;
        let mut process = spawn(Command::new(&binary).arg("-config").arg(&config))?;
        wait_port(address, &mut process)?;
        let grants = exercise(
            connect(address, mode, &cert, &public, &sid)?,
            &k,
            target,
            refused,
        )?;
        // Wrong exporter-bound proof must fail after otherwise valid TLS/REALITY.
        let mut wrong = connect(address, mode, &cert, &public, &sid)?;
        let p = proof(&k, &[0; 32], &random(16)?)?;
        wrong.send(&[p, frame(1, 1, &wire::address(target))].concat())?;
        let mut first = [0; 1];
        if wrong.read_exact(&mut first).is_ok() {
            return Err("invalid AUTH accepted".into());
        }
        println!(
            "{}",
            json!({"direction":"Rust client -> Veil server","mode":mode,"binary":binary,"result":"PASS","data_frames_sent":600,"credit_grants_received":grants,"checks":["TLS identity","Finished","exporter AUTH","echo","FIN/DONE","reuse","8 streams","FAILED recovery","RESET recovery","bad AUTH rejection"]})
        );
    }
    reverse(
        &binary,
        &folder.0,
        &cert,
        &private_file,
        &Credentials {
            key: &k,
            private: &private,
            public: &public,
            sid: &sid,
        },
        target,
    )?;
    Ok(())
}
struct Credentials<'a> {
    key: &'a [u8],
    private: &'a [u8],
    public: &'a [u8],
    sid: &'a [u8],
}
fn reverse(
    binary: &Path,
    folder: &Path,
    cert: &Path,
    keyfile: &Path,
    credentials: &Credentials<'_>,
    target: SocketAddr,
) -> Result<()> {
    let Credentials {
        key: k,
        private,
        public,
        sid,
    } = credentials;
    for mode in ["tls", "reality"] {
        let listener = TcpListener::bind("127.0.0.1:0")?;
        let remote = listener.local_addr()?;
        let local = vacant()?;
        let (cert_owned, key_owned) = (cert.to_path_buf(), keyfile.to_path_buf());
        let (k_owned, private_owned, sid_owned) = (k.to_vec(), private.to_vec(), sid.to_vec());
        // One physical session is enough to test both endpoint implementations.
        let server = thread::spawn(move || {
            let result = server::serve(
                listener,
                mode,
                &cert_owned,
                &key_owned,
                &private_owned,
                &sid_owned,
                &k_owned,
            );
            if let Err(error) = &result {
                eprintln!("Rust {mode} server: {error}");
            }
            result
        });
        let mut tls = json!({"mode":mode,"server_name":"cover.test","record_padding":true});
        if mode == "tls" {
            tls["ca_file"] = json!(cert);
        } else {
            tls["reality_public_key"] = json!(URL_SAFE_NO_PAD.encode(public));
            tls["short_id"] = json!(hex::encode(sid));
            tls["fingerprint"] = json!("chrome149");
        }
        let cfg = json!({"role":"client","listen":local.to_string(),"server":remote.to_string(),"secret":URL_SAFE_NO_PAD.encode(k),"tls":tls,"max_connections":8,"max_idle":1});
        let path = folder.join(format!("reverse-{mode}.json"));
        fs::write(&path, serde_json::to_vec(&cfg)?)?;
        let mut process = spawn(Command::new(binary).arg("-config").arg(path))?;
        wait_port(local, &mut process)?;
        for size in [64, 1048576, 17] {
            let mut io = socket(local)?;
            io.write_all(&[5, 1, 0])?;
            let mut method = [0; 2];
            io.read_exact(&mut method)?;
            if method != [5, 0] {
                return Err("SOCKS method".into());
            }
            io.write_all(&[&[5, 1, 0], wire::address(target).as_slice()].concat())?;
            let mut reply = [0; 10];
            io.read_exact(&mut reply)?;
            if reply[..4] != [5, 0, 0, 1] {
                return Err("SOCKS response".into());
            }
            let payload = vec![77; size];
            let mut writer = io.try_clone()?;
            let expected = payload.clone();
            let tx = thread::spawn(move || -> Result<()> {
                writer.write_all(&payload)?;
                writer.shutdown(Shutdown::Write)?;
                Ok(())
            });
            let mut received = Vec::new();
            io.read_to_end(&mut received)?;
            tx.join().map_err(|_| "sender panic")??;
            if received != expected {
                return Err("reverse payload/half-close".into());
            }
        }
        drop(process);
        let completed = server.join().map_err(|_| "server panic")??;
        if completed != 3 {
            return Err("reverse stream count".into());
        }
        println!(
            "{}",
            json!({"direction":"Veil client -> Rust server","mode":mode,"result":"PASS","completed":completed,"bytes":1048657,"checks":["TLS/REALITY identity","exporter AUTH","reuse","large echo","FIN/DONE"]})
        );
    }
    Ok(())
}
fn vectors() -> Result<Value> {
    let k: Vec<u8> = (0..32).collect();
    let b: Vec<u8> = (32..64).collect();
    let n: Vec<u8> = (160..176).collect();
    let xp = hex::decode("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")?;
    let rp = hex::decode("5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb")?;
    let r: Vec<u8> = (0..32).collect();
    let sid = hex::decode("0102030405060708")?;
    let pubx = x_public(&xp)?;
    let pubr = x_public(&rp)?;
    let z = x_shared(&xp, &pubr)?;
    let a = auth_key(&z, &r)?;
    let ch0 = reality::client_hello(&r, &pubx, "cover.test");
    let sid_ct = reality::session_id(&a, &r, &sid, 0x65010203, &ch0)?;
    // P-256 generator point (private scalar 1), encoded as DER SubjectPublicKeyInfo.
    let spki = hex::decode(concat!(
        "3059301306072a8648ce3d020106082a8648ce3d03010703420004",
        "6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296",
        "4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5"
    ))?;
    let cert_proof = mac(
        &a,
        &[b"Veil-v0.3 server authentication\0".as_slice(), &spki].concat(),
    )?;
    let exp = exporter(&k)?;
    Ok(
        json!({"auth":{"K":hex::encode(&k),"B":hex::encode(&b),"N":hex::encode(&n),"frame":hex::encode(proof(&k,&b,&n)?)},"exporter_sha256":{"EMS":hex::encode(&k),"B":hex::encode(exp)},"reality":{"client_private":hex::encode(&xp),"client_public":hex::encode(pubx),"server_private":hex::encode(&rp),"server_public":hex::encode(pubr),"client_random":hex::encode(r),"short_id":hex::encode(sid),"unix_time_hex":"65010203","Z":hex::encode(z),"A":hex::encode(&a),"CH0":hex::encode(ch0),"SID":hex::encode(sid_ct),"p256_spki_der":hex::encode(&spki),"subject_key_identifier":hex::encode(cert_proof)} }),
    )
}
fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("vectors") => println!("{}", serde_json::to_string_pretty(&vectors()?)?),
        Some("selftest") if args.len() == 3 => selftest(Path::new(&args[2]))?,
        _ => return Err("usage: veil-interop vectors | selftest /absolute/path/to/veil".into()),
    }
    Ok(())
}
