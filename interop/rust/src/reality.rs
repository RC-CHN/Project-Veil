//! Test-only TLS 1.3 peer: X25519 / AES-128-GCM / SHA-256, no resumption.
//! Implements REALITY authentication without a Veil/uTLS dependency.
use crate::{Result, crypto::*};
use openssl::{hash::MessageDigest, nid::Nid, pkey::Id, sha::sha256, sign::Verifier, x509::X509};
use std::{
    io::{Read, Write},
    net::TcpStream,
    time::{SystemTime, UNIX_EPOCH},
};

pub fn client_hello(random: &[u8], public: &[u8], name: &str) -> Vec<u8> {
    let sni = vector16(&[&[0], vector16(name.as_bytes()).as_slice()].concat());
    let extensions = [
        ext(0, &sni),
        ext(10, &[0, 2, 0, 29]),
        ext(13, &[0, 6, 4, 3, 8, 4, 8, 5]),
        ext(43, &[2, 3, 4]),
        ext(
            51,
            &vector16(&[&[0, 29], vector16(public).as_slice()].concat()),
        ),
        ext(16, &vector16(b"\x08http/1.1")),
    ]
    .concat();
    handshake(
        1,
        &[
            &[3, 3],
            random,
            &[32],
            &[0; 32],
            &[0, 2, 0x13, 1],
            &[1, 0],
            &vector16(&extensions),
        ]
        .concat(),
    )
}
pub fn session_id(
    key: &[u8],
    random: &[u8],
    short: &[u8],
    timestamp: u32,
    ch0: &[u8],
) -> Result<Vec<u8>> {
    let p = [&[1, 8, 1, 0], timestamp.to_be_bytes().as_slice(), short].concat();
    seal(key, &random[20..32], ch0, &p)
}
pub fn record<R: Read>(io: &mut R) -> Result<(u8, Vec<u8>, Vec<u8>)> {
    let mut h = [0; 5];
    io.read_exact(&mut h)?;
    let n = length(&h[3..]);
    if n > 16640 {
        return Err("oversized TLS record".into());
    }
    let mut b = vec![0; n];
    io.read_exact(&mut b)?;
    Ok((h[0], h.to_vec(), b))
}
fn plain(io: &mut impl Write, typ: u8, b: &[u8]) -> Result<()> {
    io.write_all(&[&[typ, 3, 3], (b.len() as u16).to_be_bytes().as_slice(), b].concat())?;
    Ok(())
}
struct Keys {
    key: Vec<u8>,
    iv: Vec<u8>,
    seq: u64,
}
impl Keys {
    fn new(secret: &[u8]) -> Result<Self> {
        Ok(Self {
            key: label(secret, "key", b"", 16)?,
            iv: label(secret, "iv", b"", 12)?,
            seq: 0,
        })
    }
    fn nonce(&mut self) -> Result<Vec<u8>> {
        let mut n = self.iv.clone();
        for (i, x) in self.seq.to_be_bytes().iter().enumerate() {
            n[4 + i] ^= x;
        }
        self.seq = self.seq.checked_add(1).ok_or("record sequence exhausted")?;
        Ok(n)
    }
    fn write(&mut self, io: &mut impl Write, typ: u8, b: &[u8]) -> Result<()> {
        let p = [b, &[typ]].concat();
        let h = [
            &[23, 3, 3],
            ((p.len() + 16) as u16).to_be_bytes().as_slice(),
        ]
        .concat();
        let nonce = self.nonce()?;
        let ct = seal(&self.key, &nonce, &h, &p)?;
        io.write_all(&h)?;
        io.write_all(&ct)?;
        Ok(())
    }
    fn read(&mut self, io: &mut impl Read) -> Result<(u8, Vec<u8>)> {
        loop {
            let (typ, h, b) = record(io)?;
            if typ == 20 && b == [1] {
                continue;
            }
            if typ != 23 {
                return Err(format!("unexpected TLS record {typ}").into());
            }
            let nonce = self.nonce()?;
            let mut p = unseal(&self.key, &nonce, &h, &b)?;
            while p.last() == Some(&0) {
                p.pop();
            }
            let typ = p.pop().ok_or("empty TLSInnerPlaintext")?;
            if typ == 21 {
                return Err(format!("TLS alert {p:?}").into());
            }
            return Ok((typ, p));
        }
    }
}
pub struct Connection {
    io: TcpStream,
    tx: Keys,
    rx: Keys,
    buffered: Vec<u8>,
    pub exporter: Vec<u8>,
}
impl Connection {
    pub fn connect(
        mut io: TcpStream,
        name: &str,
        server_public: &[u8],
        short: &[u8],
    ) -> Result<Self> {
        let random = random(32)?;
        let private = random_bytes_32()?;
        let public = x_public(&private)?;
        let a = auth_key(&x_shared(&private, server_public)?, &random)?;
        let mut ch = client_hello(&random, &public, name);
        let sid = session_id(
            &a,
            &random,
            short,
            SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as u32,
            &ch,
        )?;
        ch[39..71].copy_from_slice(&sid);
        plain(&mut io, 22, &ch)?;
        let mut pending = Vec::new();
        while pending.len() < 4 || pending.len() < 4 + length(&pending[1..4]) {
            let (t, _, b) = record(&mut io)?;
            if t != 22 {
                return Err("expected ServerHello".into());
            }
            pending.extend(b);
            if pending.len() > 65536 {
                return Err("large ServerHello".into());
            }
        }
        let sh_len = 4 + length(&pending[1..4]);
        if pending[0] != 2 || sh_len != pending.len() {
            return Err("ServerHello framing".into());
        }
        let mut p = Bytes(&pending[4..]);
        if p.take(2)? != [3, 3] {
            return Err("legacy_version".into());
        }
        p.take(32)?;
        if p.vector(1)? != sid {
            return Err("session id echo".into());
        }
        if p.take(2)? != [0x13, 1] || p.num(1)? != 0 {
            return Err("unsupported test cipher".into());
        }
        let mut exts = Bytes(p.vector(2)?);
        let mut peer = Vec::new();
        let mut tls13 = false;
        while !exts.0.is_empty() {
            let t = exts.num(2)?;
            let b = exts.vector(2)?;
            if t == 43 {
                tls13 = b == [3, 4];
            }
            if t == 51 {
                let mut b = Bytes(b);
                if b.num(2)? != 29 {
                    return Err("test requires X25519".into());
                }
                peer = b.vector(2)?.to_vec();
            }
        }
        if !tls13 || peer.len() != 32 {
            return Err("invalid TLS 1.3 server parameters".into());
        }
        let mut transcript = [ch, pending].concat();
        let early = mac(&[0; 32], &[0; 32])?;
        let hs = mac(
            &derive(&early, "derived", b"")?,
            &x_shared(&private, &peer)?,
        )?;
        let c_hs = derive(&hs, "c hs traffic", &transcript)?;
        let s_hs = derive(&hs, "s hs traffic", &transcript)?;
        let mut rx = Keys::new(&s_hs)?;
        let mut tx = Keys::new(&c_hs)?;
        let mut messages = Vec::new();
        let mut cert = None;
        let mut verified = false;
        let mut extensions_seen = false;
        loop {
            while messages.len() < 4 || messages.len() < 4 + length(&messages[1..4]) {
                let (typ, b) = rx.read(&mut io)?;
                if typ != 22 {
                    return Err("expected encrypted handshake".into());
                }
                messages.extend(b);
                if messages.len() > 1 << 20 {
                    return Err("handshake exceeds test bound".into());
                }
            }
            let n = 4 + length(&messages[1..4]);
            let message: Vec<u8> = messages.drain(..n).collect();
            let mut p = Bytes(&message[4..]);
            match message[0] {
                8 if !extensions_seen && cert.is_none() => {
                    extensions_seen = true;
                    let mut e = Bytes(p.vector(2)?);
                    while !e.0.is_empty() {
                        let t = e.num(2)?;
                        let b = e.vector(2)?;
                        if t == 16 && b != b"\x00\x09\x08http/1.1" {
                            return Err("unexpected ALPN".into());
                        }
                    }
                }
                11 if extensions_seen && cert.is_none() => {
                    if !p.vector(1)?.is_empty() {
                        return Err("certificate request context".into());
                    }
                    let mut chain = Bytes(p.vector(3)?);
                    let leaf = X509::from_der(chain.vector(3)?)?;
                    chain.vector(2)?;
                    let key = leaf.public_key()?;
                    if key.id() != Id::EC
                        || key.ec_key()?.group().curve_name() != Some(Nid::X9_62_PRIME256V1)
                    {
                        return Err("REALITY key is not P-256".into());
                    }
                    let h = mac(
                        &a,
                        &[
                            b"Veil-v0.3 server authentication\0".as_slice(),
                            &key.public_key_to_der()?,
                        ]
                        .concat(),
                    )?;
                    if leaf
                        .subject_key_id()
                        .is_none_or(|id| !same(&h, id.as_slice()))
                        || leaf.signature_algorithm().object().nid() != Nid::ECDSA_WITH_SHA256
                        || !leaf.verify(&key)?
                    {
                        return Err("REALITY certificate proof".into());
                    }
                    cert = Some(key);
                }
                15 if cert.is_some() && !verified => {
                    if p.num(2)? != 0x0403 {
                        return Err("CertificateVerify algorithm".into());
                    }
                    let input = [
                        &[32; 64],
                        b"TLS 1.3, server CertificateVerify\0".as_slice(),
                        &sha256(&transcript),
                    ]
                    .concat();
                    if !Verifier::new(MessageDigest::sha256(), cert.as_ref().unwrap())?
                        .verify_oneshot(p.vector(2)?, &input)?
                    {
                        return Err("CertificateVerify signature".into());
                    }
                    verified = true;
                }
                20 if verified => {
                    let expected = mac(&label(&s_hs, "finished", b"", 32)?, &sha256(&transcript))?;
                    if !same(&expected, p.0) {
                        return Err("server Finished".into());
                    }
                    transcript.extend(&message);
                    break;
                }
                t => return Err(format!("unexpected handshake type/order {t}").into()),
            }
            transcript.extend(message);
        }
        if !messages.is_empty() {
            return Err("extra handshake bytes".into());
        }
        let master = mac(&derive(&hs, "derived", b"")?, &[0; 32])?;
        let exp = exporter(&derive(&master, "exp master", &transcript)?)?;
        let ap_tx = Keys::new(&derive(&master, "c ap traffic", &transcript)?)?;
        let ap_rx = Keys::new(&derive(&master, "s ap traffic", &transcript)?)?;
        let fin = handshake(
            20,
            &mac(&label(&c_hs, "finished", b"", 32)?, &sha256(&transcript))?,
        );
        plain(&mut io, 20, &[1])?;
        tx.write(&mut io, 22, &fin)?;
        Ok(Self {
            io,
            tx: ap_tx,
            rx: ap_rx,
            buffered: Vec::new(),
            exporter: exp,
        })
    }
    pub fn send(&mut self, p: &[u8]) -> Result<()> {
        for p in p.chunks(16384) {
            self.tx.write(&mut self.io, 23, p)?;
        }
        Ok(())
    }
    pub fn read_exact(&mut self, p: &mut [u8]) -> Result<()> {
        while self.buffered.len() < p.len() {
            let (t, b) = self.rx.read(&mut self.io)?;
            if t != 23 {
                return Err("post-handshake message unsupported by minimal test client".into());
            }
            self.buffered.extend(b);
        }
        p.copy_from_slice(&self.buffered[..p.len()]);
        self.buffered.drain(..p.len());
        Ok(())
    }
}
fn random_bytes_32() -> Result<Vec<u8>> {
    random(32)
}
