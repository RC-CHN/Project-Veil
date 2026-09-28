use crate::{Result, crypto::*, reality};
use openssl::ssl::SslStream;
use std::{
    collections::BTreeMap,
    io::{Cursor, Read, Write},
    net::{SocketAddr, TcpStream},
};

#[derive(Debug)]
pub struct Prefixed {
    pub saved: Cursor<Vec<u8>>,
    pub stream: TcpStream,
}
impl Read for Prefixed {
    fn read(&mut self, p: &mut [u8]) -> std::io::Result<usize> {
        let n = self.saved.read(p)?;
        if n > 0 { Ok(n) } else { self.stream.read(p) }
    }
}
impl Write for Prefixed {
    fn write(&mut self, p: &[u8]) -> std::io::Result<usize> {
        self.stream.write(p)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        self.stream.flush()
    }
}
pub enum Channel {
    Tls(SslStream<Prefixed>),
    Reality(reality::Connection),
}
impl Channel {
    pub fn exporter(&self) -> Result<Vec<u8>> {
        match self {
            Self::Tls(s) => {
                let mut b = vec![0; 32];
                s.ssl()
                    .export_keying_material(&mut b, "EXPORTER-Veil-v0.3", Some(b""))?;
                Ok(b)
            }
            Self::Reality(s) => Ok(s.exporter.clone()),
        }
    }
    pub fn send(&mut self, p: &[u8]) -> Result<()> {
        match self {
            Self::Tls(s) => {
                s.write_all(p)?;
                Ok(())
            }
            Self::Reality(s) => s.send(p),
        }
    }
    pub fn read_exact(&mut self, p: &mut [u8]) -> Result<()> {
        match self {
            Self::Tls(s) => {
                s.read_exact(p)?;
                Ok(())
            }
            Self::Reality(s) => s.read_exact(p),
        }
    }
}
pub fn address(a: SocketAddr) -> Vec<u8> {
    match a {
        SocketAddr::V4(a) => [
            &[1],
            a.ip().octets().as_slice(),
            a.port().to_be_bytes().as_slice(),
        ]
        .concat(),
        SocketAddr::V6(a) => [
            &[4],
            a.ip().octets().as_slice(),
            a.port().to_be_bytes().as_slice(),
        ]
        .concat(),
    }
}
pub fn frame(t: u8, id: u32, p: &[u8]) -> Vec<u8> {
    [
        &[t],
        id.to_be_bytes().as_slice(),
        u24(p.len()).as_slice(),
        p,
    ]
    .concat()
}
pub fn read_frame(io: &mut Channel) -> Result<(u8, u32, Vec<u8>)> {
    let mut h = [0; 8];
    io.read_exact(&mut h)?;
    let (t, id, n) = header(&h)?;
    let mut p = vec![0; n];
    io.read_exact(&mut p)?;
    Ok((t, id, p))
}
pub fn header(h: &[u8; 8]) -> Result<(u8, u32, usize)> {
    let id = u32::from_be_bytes(h[1..5].try_into()?);
    let n = length(&h[5..]);
    let valid = match h[0] {
        1 => (1..=512).contains(&n),
        2 | 5 | 6 | 8 => n == 0,
        3 => n == 1,
        4 => (1..=32768).contains(&n),
        7 => n == 2,
        _ => false,
    };
    if id == 0 || id.is_multiple_of(2) || !valid {
        return Err("invalid MUX header".into());
    }
    Ok((h[0], id, n))
}
#[derive(Default)]
struct Stream {
    opened: bool,
    fin: bool,
    local_fin: bool,
    done: bool,
    failed: Option<u8>,
    credit: usize,
    consumed: usize,
    data: Vec<u8>,
}
pub struct Peer {
    pub io: Channel,
    streams: BTreeMap<u32, Stream>,
    pub grants: usize,
    high: u32,
}
impl Peer {
    pub fn new(io: Channel) -> Self {
        Self {
            io,
            streams: BTreeMap::new(),
            grants: 0,
            high: 0,
        }
    }
    pub fn open(&mut self, id: u32, target: SocketAddr, prefix: &[u8]) -> Result<()> {
        if id <= self.high || id.is_multiple_of(2) || self.streams.len() >= 8 {
            return Err("invalid local OPEN".into());
        }
        self.high = id;
        self.streams.insert(
            id,
            Stream {
                credit: 256,
                ..Default::default()
            },
        );
        self.io
            .send(&[prefix, frame(1, id, &address(target)).as_slice()].concat())?;
        while !self.streams[&id].opened && self.streams[&id].failed.is_none() {
            self.event()?;
        }
        Ok(())
    }
    pub fn failed(&mut self, id: u32) -> Option<u8> {
        let result = self.streams[&id].failed;
        if result.is_some() {
            self.streams.remove(&id);
        }
        result
    }
    pub fn event(&mut self) -> Result<()> {
        let mut h = [0; 8];
        self.io.read_exact(&mut h)?;
        let (typ, id, n) = header(&h)?;
        if id > self.high {
            return Err("unknown future stream".into());
        }
        let mut p = vec![0; n];
        self.io.read_exact(&mut p)?;
        let Some(s) = self.streams.get_mut(&id) else {
            return Ok(());
        };
        match typ {
            2 if !s.opened => s.opened = true,
            3 if !s.opened => s.failed = Some(p[0]),
            4 if s.opened && !s.fin => {
                s.data.extend(p);
                s.consumed += 1;
                if s.consumed >= 32 {
                    self.io
                        .send(&frame(7, id, &(s.consumed as u16).to_be_bytes()))?;
                    s.consumed = 0;
                }
            }
            5 if s.opened && !s.fin => s.fin = true,
            8 if s.fin && s.local_fin && !s.done => s.done = true,
            7 => {
                let k = length(&p);
                if k == 0 || k > 256 - s.credit {
                    return Err("credit overflow".into());
                }
                s.credit += k;
                self.grants += k;
            }
            _ => return Err(format!("unexpected frame {typ} on stream {id}").into()),
        }
        Ok(())
    }
    pub fn send(&mut self, id: u32, p: &[u8]) -> Result<()> {
        for p in p.chunks(32768) {
            while self.streams[&id].credit == 0 {
                self.event()?;
            }
            let s = self.streams.get_mut(&id).ok_or("no stream")?;
            if !s.opened || s.local_fin {
                return Err("DATA state".into());
            }
            s.credit -= 1;
            self.io.send(&frame(4, id, p))?;
        }
        Ok(())
    }
    pub fn finish(&mut self, id: u32, expected: &[u8]) -> Result<()> {
        self.streams.get_mut(&id).ok_or("no stream")?.local_fin = true;
        self.io.send(&frame(5, id, b""))?;
        while !self.streams[&id].done {
            self.event()?;
        }
        let stream = self.streams.remove(&id).unwrap();
        if stream.data != expected {
            return Err("echo payload differs".into());
        }
        Ok(())
    }
    pub fn echo(&mut self, id: u32, expected: &[u8]) -> Result<()> {
        while self.streams[&id].data.len() < expected.len() {
            self.event()?;
        }
        if self.streams[&id].data != expected {
            return Err("echo payload differs".into());
        }
        Ok(())
    }
    pub fn reset(&mut self, id: u32) -> Result<()> {
        self.io.send(&frame(6, id, b""))?;
        self.streams.remove(&id);
        Ok(())
    }
}
