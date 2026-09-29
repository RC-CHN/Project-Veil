use ring::{
    hmac,
    rand::{SecureRandom, SystemRandom},
};
use std::{
    io,
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
};

pub const OPEN: u8 = 1;
pub const OPENED: u8 = 2;
pub const FAILED: u8 = 3;
pub const DATA: u8 = 4;
pub const FIN: u8 = 5;
pub const RESET: u8 = 6;
pub const CREDIT: u8 = 7;
pub const DONE: u8 = 8;
pub const BLOCK: usize = 32768;
pub const WINDOW: u16 = 256;
pub const MAX_STREAMS: usize = 8;

pub fn invalid() -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "invalid Veil frame or state")
}

pub fn header(h: [u8; 8]) -> io::Result<(u8, u32, usize)> {
    let id = u32::from_be_bytes(h[1..5].try_into().unwrap());
    let n = (usize::from(h[5]) << 16) | (usize::from(h[6]) << 8) | usize::from(h[7]);
    let valid = match h[0] {
        OPEN => (1..=512).contains(&n),
        OPENED | FIN | RESET | DONE => n == 0,
        FAILED => n == 1,
        CREDIT => n == 2,
        DATA => (1..=BLOCK).contains(&n),
        _ => false,
    };
    if id == 0 || id & 1 == 0 || !valid {
        return Err(invalid());
    }
    Ok((h[0], id, n))
}

pub fn frame(out: &mut Vec<u8>, kind: u8, id: u32, payload: &[u8]) {
    out.push(kind);
    out.extend_from_slice(&id.to_be_bytes());
    let n = payload.len() as u32;
    out.extend_from_slice(&n.to_be_bytes()[1..]);
    out.extend_from_slice(payload);
}

pub fn auth_with_nonce(key: &[u8; 32], exporter: &[u8; 32], nonce: &[u8; 16]) -> [u8; 53] {
    let mut out = [0; 53];
    out[..5].copy_from_slice(&[1, 0, 0, 49, 3]);
    out[5..21].copy_from_slice(nonce);
    let mut h = hmac::Context::with_key(&hmac::Key::new(hmac::HMAC_SHA256, key));
    h.update(b"Veil-v0.3 client authentication\0");
    h.update(exporter);
    h.update(&out[4..21]);
    out[21..].copy_from_slice(h.sign().as_ref());
    out
}

pub fn auth(key: &[u8; 32], exporter: &[u8; 32]) -> io::Result<[u8; 53]> {
    let mut nonce = [0; 16];
    SystemRandom::new()
        .fill(&mut nonce)
        .map_err(|_| io::Error::other("random source failed"))?;
    Ok(auth_with_nonce(key, exporter, &nonce))
}

pub fn verify_auth(key: &[u8; 32], exporter: &[u8; 32], frame: &[u8; 53]) -> io::Result<()> {
    if frame[..5] != [1, 0, 0, 49, 3] {
        return Err(invalid());
    }
    let msg = [
        b"Veil-v0.3 client authentication\0".as_slice(),
        exporter,
        &frame[4..21],
    ]
    .concat();
    hmac::verify(&hmac::Key::new(hmac::HMAC_SHA256, key), &msg, &frame[21..]).map_err(|_| invalid())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    pub host: String,
    pub port: u16,
}

impl Target {
    pub fn decode(p: &[u8]) -> io::Result<Self> {
        let host = match p.first() {
            Some(1) if p.len() == 7 => {
                Ipv4Addr::from(<[u8; 4]>::try_from(&p[1..5]).unwrap()).to_string()
            }
            Some(4) if p.len() == 19 => {
                Ipv6Addr::from(<[u8; 16]>::try_from(&p[1..17]).unwrap()).to_string()
            }
            Some(3) if p.len() >= 5 && p[1] != 0 && p.len() == p[1] as usize + 4 => {
                let b = &p[2..p.len() - 2];
                if b.iter()
                    .any(|c| *c <= 32 || matches!(*c, 127 | b':' | b'/' | b'\\') || !c.is_ascii())
                {
                    return Err(invalid());
                }
                String::from_utf8(b.to_vec()).map_err(|_| invalid())?
            }
            _ => return Err(invalid()),
        };
        let port = u16::from_be_bytes(p[p.len() - 2..].try_into().unwrap());
        if port == 0 {
            return Err(invalid());
        }
        Ok(Self { host, port })
    }

    pub fn encode(&self) -> io::Result<Vec<u8>> {
        let mut p = Vec::new();
        match self.host.parse::<IpAddr>() {
            Ok(IpAddr::V4(ip)) => {
                p.push(1);
                p.extend_from_slice(&ip.octets());
            }
            Ok(IpAddr::V6(ip)) => {
                p.push(4);
                p.extend_from_slice(&ip.octets());
            }
            Err(_) => {
                if self.host.len() > 255 {
                    return Err(invalid());
                }
                p.extend_from_slice(&[3, self.host.len() as u8]);
                p.extend_from_slice(self.host.as_bytes());
            }
        }
        p.extend_from_slice(&self.port.to_be_bytes());
        Self::decode(&p)?;
        Ok(p)
    }
}

impl From<SocketAddr> for Target {
    fn from(a: SocketAddr) -> Self {
        Self {
            host: a.ip().to_string(),
            port: a.port(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn auth_vector_and_binding() {
        let k = std::array::from_fn(|i| i as u8);
        let b = std::array::from_fn(|i| (i + 32) as u8);
        let n = std::array::from_fn(|i| (i + 160) as u8);
        let a = auth_with_nonce(&k, &b, &n);
        let hex: String = a.iter().map(|b| format!("{b:02x}")).collect();
        assert_eq!(
            hex,
            "0100003103a0a1a2a3a4a5a6a7a8a9aaabacadaeaf9e1d50ff1133d04ce8400dacfaab3a595cd58deeebb918e97ea7f9b38710268b"
        );
        verify_auth(&k, &b, &a).unwrap();
        assert!(verify_auth(&k, &[0; 32], &a).is_err());
    }
    #[test]
    fn validate_before_allocation() {
        for (kind, n, ok) in [
            (DATA, 32768, true),
            (DATA, 32769, false),
            (DATA, 0, false),
            (CREDIT, 2, true),
            (OPEN, 513, false),
            (99, 0, false),
        ] {
            let mut out = Vec::new();
            frame(&mut out, kind, 1, &vec![0; n]);
            assert_eq!(header(out[..8].try_into().unwrap()).is_ok(), ok);
            out[4] = 2;
            assert!(header(out[..8].try_into().unwrap()).is_err());
        }
    }
    #[test]
    fn addresses() {
        for s in ["127.0.0.1:443", "[::1]:80"] {
            let a = Target::from(s.parse::<SocketAddr>().unwrap());
            assert_eq!(Target::decode(&a.encode().unwrap()).unwrap(), a);
        }
        for host in ["", "bad/name", "bad:80", "a\0b", "é.test"] {
            assert!(
                Target {
                    host: host.into(),
                    port: 443
                }
                .encode()
                .is_err()
            );
        }
    }
}
