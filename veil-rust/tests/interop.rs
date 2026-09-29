//! Optional real-process interoperability: VEIL_GO_BINARY=/absolute/path/to/veil.
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde_json::json;
use std::{
    fs, io,
    net::{SocketAddr, TcpListener as StdListener},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};
use veil_rust::wire::Target;

struct Process(Child);
impl Drop for Process {
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
fn vacant() -> SocketAddr {
    StdListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
}
fn launch(binary: &Path, config: &Path) -> Process {
    Process(
        Command::new(binary)
            .args(["-config"])
            .arg(config)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap(),
    )
}
async fn ready(addr: SocketAddr, p: &mut Process) {
    for _ in 0..500 {
        assert!(p.0.try_wait().unwrap().is_none(), "proxy exited");
        if TcpStream::connect(addr).await.is_ok() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("proxy readiness timeout");
}
async fn connect(proxy: SocketAddr, target: SocketAddr, http: bool) -> io::Result<TcpStream> {
    let mut tcp = TcpStream::connect(proxy).await?;
    tcp.set_nodelay(true)?;
    if http {
        tcp.write_all(format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n\r\n").as_bytes())
            .await?;
        let mut header = Vec::new();
        while !header.ends_with(b"\r\n\r\n") {
            header.push(tcp.read_u8().await?);
            if header.len() > 8192 {
                return Err(io::Error::other("HTTP header"));
            }
        }
        if !header.starts_with(b"HTTP/1.1 200 ") {
            return Err(io::Error::other("HTTP rejected"));
        }
    } else {
        tcp.write_all(&[5, 1, 0]).await?;
        let mut method = [0; 2];
        tcp.read_exact(&mut method).await?;
        if method != [5, 0] {
            return Err(io::Error::other("SOCKS method"));
        }
        tcp.write_all(&[&[5, 1, 0][..], &Target::from(target).encode()?].concat())
            .await?;
        let mut reply = [0; 10];
        tcp.read_exact(&mut reply).await?;
        if reply[..4] != [5, 0, 0, 1] {
            return Err(io::Error::other("SOCKS rejected"));
        }
    }
    Ok(tcp)
}
async fn exercise(proxy: SocketAddr, target: SocketAddr, http: bool, size: usize, seed: usize) {
    let mut tcp = connect(proxy, target, http).await.unwrap();
    let payload: Vec<_> = (0..size).map(|i| ((i * 131 + seed) % 251) as u8).collect();
    tcp.write_all(&payload).await.unwrap();
    tcp.shutdown().await.unwrap();
    let mut got = Vec::new();
    tcp.read_to_end(&mut got).await.unwrap();
    assert_eq!(got, payload, "payload corruption/half-close");
}
#[tokio::test]
async fn real_go_rust_matrix() {
    let Ok(go) = std::env::var("VEIL_GO_BINARY") else {
        eprintln!("set VEIL_GO_BINARY to run Go/Rust process interop");
        return;
    };
    tokio::time::timeout(Duration::from_secs(90),async {
        let rust=Path::new(env!("CARGO_BIN_EXE_veil-rust"));let go=Path::new(&go);
        let folder=Folder(std::env::temp_dir().join(format!("veil-rust-interop-{}-{}",std::process::id(),rand::random::<u64>())));fs::create_dir(&folder.0).unwrap();
        let cert=folder.0.join("cert.pem");let key=folder.0.join("key.pem");
        assert!(Command::new("openssl").args(["req","-x509","-newkey","ec","-pkeyopt","ec_paramgen_curve:P-256","-nodes","-days","1","-subj","/CN=cover.test","-addext","subjectAltName=DNS:cover.test","-addext","basicConstraints=critical,CA:FALSE","-keyout"]).arg(&key).arg("-out").arg(&cert).stdout(Stdio::null()).stderr(Stdio::null()).status().unwrap().success());
        let listener=TcpListener::bind("127.0.0.1:0").await.unwrap();let target=listener.local_addr().unwrap();
        let backend=tokio::spawn(async move {loop{let (mut tcp,_)=listener.accept().await.unwrap();tokio::spawn(async move {
            let mut bytes=Vec::new();tcp.read_to_end(&mut bytes).await.unwrap();
            // Target EOF comes only after all request data and client half-close.
            tokio::time::sleep(Duration::from_millis(3)).await;tcp.write_all(&bytes).await.unwrap();tcp.shutdown().await.unwrap();
        });}});
        for (cb,sb) in [(rust,go),(go,rust),(rust,rust)] {
            let sp=vacant();let cp=vacant();let secret=URL_SAFE_NO_PAD.encode([42;32]);
            let server=json!({"role":"server","listen":sp.to_string(),"secret":secret,"max_connections":64,"max_idle":8,"tls":{"mode":"tls","server_name":"cover.test","certificate":cert,"private_key_file":key}});
            let client=json!({"role":"client","listen":cp.to_string(),"server":sp.to_string(),"secret":secret,"max_connections":64,"max_idle":8,"tls":{"mode":"tls","server_name":"cover.test","ca_file":cert}});
            let sf=folder.0.join("server.json");let cf=folder.0.join("client.json");fs::write(&sf,server.to_string()).unwrap();fs::write(&cf,client.to_string()).unwrap();
            let mut server=launch(sb,&sf);ready(sp,&mut server).await;let mut client_proc=launch(cb,&cf);ready(cp,&mut client_proc).await;
            exercise(cp,target,false,37,0).await;exercise(cp,target,false,12<<20,7).await;exercise(cp,target,false,17,1).await;
            if cb==rust {exercise(cp,target,true,65537,9).await;}
            // More than eight independent streams also exercises pool expansion.
            let mut jobs=tokio::task::JoinSet::new();
            for i in 0..24 {jobs.spawn(exercise(cp,target,false,1<<20,i));}
            while let Some(result)=jobs.join_next().await{result.unwrap();}
            let refused=vacant();assert!(connect(cp,refused,false).await.is_err());exercise(cp,target,false,64,3).await;
            // Kill an unfinished upstream session. Rust explicitly sends RST;
            // the current Go frontend closes promptly but may expose TCP EOF.
            let mut live = connect(cp, target, false).await.unwrap();
            live.write_all(b"unfinished request").await.unwrap();
            server.0.kill().unwrap(); server.0.wait().unwrap();
            let mut one = [0; 1];
            let ended = tokio::time::timeout(Duration::from_secs(3), live.read(&mut one)).await.unwrap();
            if cb == rust { assert!(ended.is_err(), "Rust must propagate aborted tunnel as TCP error"); }
            else { assert!(matches!(ended, Ok(0) | Err(_)), "Go must close promptly"); }
            drop(live);
            server = launch(sb, &sf); ready(sp, &mut server).await;
            // Dead sessions are removed from the pool on the next open.
            exercise(cp, target, false, 64, 11).await;
            drop(client_proc);
            // A bad shared secret must never expose the target to local clients.
            let mut wrong=client.clone();wrong["secret"]=json!(URL_SAFE_NO_PAD.encode([43;32]));fs::write(&cf,wrong.to_string()).unwrap();
            let mut bad=launch(cb,&cf);ready(cp,&mut bad).await;assert!(connect(cp,target,false).await.is_err());drop(bad);
            if cb==rust {
                let mut wrong=client.clone();wrong["tls"]["server_name"]=json!("wrong.test");fs::write(&cf,wrong.to_string()).unwrap();
                let mut bad=launch(cb,&cf);ready(cp,&mut bad).await;assert!(connect(cp,target,false).await.is_err());
            }
            eprintln!("PASS {} -> {}: payload, credits, half-close, 24 streams, refusal recovery, upstream kill/reconnect, bad AUTH",cb.display(),sb.display());
        }
        backend.abort();
    }).await.expect("interop hung");
}
