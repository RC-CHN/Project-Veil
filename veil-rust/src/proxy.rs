use crate::{
    config::Config,
    mux::{Session, Stream},
    transport,
    wire::{self, Target},
};
use std::{io, sync::Arc, time::Duration};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::{Mutex, Semaphore},
    task::JoinSet,
};
use tokio_rustls::TlsConnector;

pub struct Client {
    config: Config,
    key: [u8; 32],
    tls: TlsConnector,
    sessions: Mutex<Vec<Session>>,
}
impl Client {
    pub fn new(config: Config) -> io::Result<Self> {
        config.validate()?;
        Ok(Self {
            key: config.key()?,
            tls: transport::client(&config.tls)?,
            config,
            sessions: Mutex::new(Vec::new()),
        })
    }
    pub async fn open(&self, target: &Target) -> io::Result<Stream> {
        let mut stream = {
            let mut sessions = self.sessions.lock().await;
            sessions.retain(|s| !s.snapshot().1);
            let mut idle = 0;
            sessions.retain(|s| {
                if s.snapshot().0 != 0 {
                    return true;
                }
                idle += 1;
                idle <= self.config.max_idle
            });
            let best = sessions
                .iter()
                .enumerate()
                .filter(|(_, s)| s.snapshot().0 < 8)
                .min_by_key(|(_, s)| (s.bulk(), s.snapshot().0))
                .map(|(i, _)| i);
            let want_new = best.is_none_or(|i| {
                sessions.len() < 2.min(self.config.max_connections)
                    && (sessions[i].bulk() || sessions[i].snapshot().0 >= 4)
            });
            if !want_new || sessions.len() >= self.config.max_connections && best.is_some() {
                sessions[best.unwrap()].reserve(target)?
            } else {
                if sessions.len() >= self.config.max_connections {
                    return Err(io::ErrorKind::WouldBlock.into());
                }
                let (io, prefix) = transport::connect(
                    &self.tls,
                    &self.config.server,
                    &self.config.tls.server_name,
                    &self.key,
                )
                .await?;
                let (session, _) = Session::start(
                    io,
                    false,
                    prefix,
                    self.config.traffic.clone(),
                    self.config.idle(),
                    Duration::from_secs(self.config.pool_seconds),
                )?;
                let stream = session.reserve(target)?;
                sessions.push(session);
                stream
            }
        };
        tokio::time::timeout(transport::HANDSHAKE, stream.ready()).await??;
        Ok(stream)
    }
}

pub async fn serve(config: Config) -> io::Result<()> {
    config.validate()?;
    let listener = TcpListener::bind(&config.listen).await?;
    eprintln!(
        "Veil Rust v0.3/T {} listening on {}",
        config.role,
        listener.local_addr()?
    );
    let mut tasks = JoinSet::new();
    if config.role == "client" {
        let client = Arc::new(Client::new(config)?);
        let limit = Arc::new(Semaphore::new(client.config.max_connections));
        loop {
            tokio::select! {
                result=listener.accept()=>{
                    let (tcp,_)=result?;
                    let Ok(permit)=limit.clone().try_acquire_owned() else {continue};
                    let client=client.clone();
                    tasks.spawn(async move {let _permit=permit;if let Err(e)=inbound(tcp,&client).await{eprintln!("inbound: {e}");}});
                }
                _=tasks.join_next(),if !tasks.is_empty()=>{}
                _=tokio::signal::ctrl_c()=>break,
            }
        }
    } else {
        let acceptor = transport::server(&config.tls)?;
        let key = config.key()?;
        let limit = Arc::new(Semaphore::new(config.max_connections));
        loop {
            tokio::select! {
                result=listener.accept()=>{
                    let (tcp,_)=result?;
                    let Ok(permit)=limit.clone().try_acquire_owned() else {continue};
                    let a=acceptor.clone();let cfg=config.clone();
                    tasks.spawn(async move {
                        let _permit=permit;
                        let result=async {
                            let io=transport::accept(&a,tcp,&key).await?;
                            let (session,mut incoming)=Session::start(io,true,Vec::new(),cfg.traffic,Duration::from_secs(cfg.idle_seconds),Duration::from_secs(cfg.pool_seconds*2))?;
                            let mut relays=JoinSet::new();
                            loop {
                                tokio::select! {
                                    next=incoming.recv()=>match next {Some((stream,address))=>{relays.spawn(outbound(stream,address));},None=>break},
                                    _=relays.join_next(),if !relays.is_empty()=>{},
                                }
                            }
                            relays.shutdown().await;session.close();Ok::<_,io::Error>(())
                        }.await;
                        if let Err(e)=result{eprintln!("session: {e}");}
                    });
                }
                _=tasks.join_next(),if !tasks.is_empty()=>{}
                _=tokio::signal::ctrl_c()=>break,
            }
        }
    }
    tasks.shutdown().await;
    Ok(())
}
async fn outbound(mut stream: Stream, address: Vec<u8>) -> io::Result<()> {
    let target = match Target::decode(&address) {
        Ok(t) => t,
        Err(_) => {
            stream.reject(1)?;
            return Ok(());
        }
    };
    let dial = tokio::time::timeout(transport::HANDSHAKE, dial_target(&target));
    let result = tokio::select! { _=stream.cancelled()=>return Ok(()),r=dial=>r };
    let mut tcp = match result {
        Ok(Ok(tcp)) => tcp,
        Ok(Err(code)) => {
            stream.reject(code)?;
            return Ok(());
        }
        Err(_) => {
            stream.reject(4)?;
            return Ok(());
        }
    };
    tcp.set_nodelay(true)?;
    stream.accept()?;
    relay(&mut tcp, &mut stream).await
}

async fn dial_target(target: &Target) -> Result<TcpStream, u8> {
    let addresses: Vec<_> = tokio::net::lookup_host((target.host.as_str(), target.port))
        .await
        .map_err(|_| 2u8)?
        .collect();
    if addresses.is_empty() {
        return Err(2);
    }
    TcpStream::connect(&addresses[..])
        .await
        .map_err(|e| match e.kind() {
            io::ErrorKind::ConnectionRefused => 3,
            io::ErrorKind::TimedOut => 4,
            _ => 1,
        })
}

async fn relay(tcp: &mut TcpStream, stream: &mut Stream) -> io::Result<()> {
    let result = async {
        tokio::io::copy_bidirectional_with_sizes(tcp, stream, 128 * 1024 - 32, 128 * 1024 - 32)
            .await?;
        stream.finish().await
    }
    .await;
    if result.is_err() {
        // A failed logical/physical stream must not look like a clean TCP EOF.
        let _ = socket2::SockRef::from(&*tcp).set_linger(Some(Duration::ZERO));
    }
    result
}

/// Mixed SOCKS5 CONNECT and HTTP CONNECT listener; no host proxy/routing changes.
async fn inbound(mut tcp: TcpStream, client: &Client) -> io::Result<()> {
    tcp.set_nodelay(true)?;
    let (target, http) =
        tokio::time::timeout(transport::HANDSHAKE, parse_inbound(&mut tcp)).await??;
    let mut stream = match client.open(&target).await {
        Ok(s) => s,
        Err(e) => {
            let reply: &[u8] = if http {
                b"HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
            } else {
                &[5, 5, 0, 1, 0, 0, 0, 0, 0, 0]
            };
            let _ = tcp.write_all(reply).await;
            return Err(e);
        }
    };
    tcp.write_all(if http {
        b"HTTP/1.1 200 Connection Established\r\n\r\n"
    } else {
        &[5, 0, 0, 1, 0, 0, 0, 0, 0, 0]
    })
    .await?;
    relay(&mut tcp, &mut stream).await
}
async fn parse_inbound(tcp: &mut TcpStream) -> io::Result<(Target, bool)> {
    let first = tcp.read_u8().await?;
    if first == 5 {
        let n = tcp.read_u8().await? as usize;
        let mut methods = vec![0; n];
        tcp.read_exact(&mut methods).await?;
        if !methods.contains(&0) {
            tcp.write_all(&[5, 255]).await?;
            return Err(wire::invalid());
        }
        tcp.write_all(&[5, 0]).await?;
        let mut head = [0; 4];
        tcp.read_exact(&mut head).await?;
        if head[..3] != [5, 1, 0] {
            return Err(wire::invalid());
        }
        let mut p = vec![head[3]];
        let n = match head[3] {
            1 => 6,
            4 => 18,
            3 => {
                let n = tcp.read_u8().await?;
                p.push(n);
                n as usize + 2
            }
            _ => return Err(wire::invalid()),
        };
        let start = p.len();
        p.resize(start + n, 0);
        tcp.read_exact(&mut p[start..]).await?;
        Ok((Target::decode(&p)?, false))
    } else {
        let mut head = vec![first];
        while !head.ends_with(b"\r\n\r\n") {
            if head.len() >= 8192 {
                return Err(wire::invalid());
            }
            head.push(tcp.read_u8().await?);
        }
        let text = std::str::from_utf8(&head).map_err(|_| wire::invalid())?;
        let line = text.split("\r\n").next().ok_or_else(wire::invalid)?;
        let parts: Vec<_> = line.split(' ').collect();
        if parts.len() != 3 || parts[0] != "CONNECT" || !matches!(parts[2], "HTTP/1.1" | "HTTP/1.0")
        {
            return Err(wire::invalid());
        }
        let (host, port) = parts[1].rsplit_once(':').ok_or_else(wire::invalid)?;
        let host = host
            .strip_prefix('[')
            .and_then(|s| s.strip_suffix(']'))
            .unwrap_or(host)
            .to_owned();
        let target = Target {
            host,
            port: port.parse().map_err(|_| wire::invalid())?,
        };
        target.encode()?;
        Ok((target, true))
    }
}
