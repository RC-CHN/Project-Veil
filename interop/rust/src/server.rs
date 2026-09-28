//! Loopback-only conformance server. Not a production relay.
use crate::{Result, crypto::*, reality, wire};
use openssl::{
    asn1::{Asn1Object, Asn1OctetString, Asn1Time},
    bn::BigNum,
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::PKey,
    ssl::{AlpnError, SslAcceptor, SslMethod, SslOptions, SslVersion, select_next_proto},
    x509::{X509, X509Extension, X509NameBuilder},
};
use std::{
    collections::BTreeMap,
    io::{Read, Write},
    net::{Shutdown, TcpListener, TcpStream},
    path::Path,
    time::{SystemTime, UNIX_EPOCH},
};

fn hello(io: &mut TcpStream) -> Result<(Vec<u8>, Vec<u8>)> {
    let mut saved = Vec::new();
    let mut ch = Vec::new();
    while ch.len() < 4 || ch.len() < 4 + length(&ch[1..4]) {
        let (t, h, b) = reality::record(io)?;
        if t != 22 {
            return Err("expected ClientHello".into());
        }
        saved.extend(h);
        saved.extend(&b);
        ch.extend(b);
        if ch.len() > 1 << 20 {
            return Err("ClientHello bound".into());
        }
    }
    if ch[0] != 1 || ch.len() != 4 + length(&ch[1..4]) {
        return Err("ClientHello framing".into());
    }
    Ok((saved, ch))
}
fn authentication(ch: &[u8], private: &[u8], short: &[u8]) -> Result<(Vec<u8>, Vec<u8>)> {
    let mut b = Bytes(&ch[4..]);
    if b.take(2)? != [3, 3] {
        return Err("legacy version".into());
    }
    let random = b.take(32)?;
    let sid = b.vector(1)?;
    if sid.len() != 32 {
        return Err("session id length".into());
    }
    b.vector(2)?;
    b.vector(1)?;
    let mut exts = Bytes(b.vector(2)?);
    let mut public = Vec::new();
    let mut hybrid = Vec::new();
    let mut sni = false;
    let mut tls13 = false;
    while !exts.0.is_empty() {
        let t = exts.num(2)?;
        let v = exts.vector(2)?;
        match t {
            0 => {
                let mut names = Bytes(v);
                let mut names = Bytes(names.vector(2)?);
                if names.num(1)? == 0 {
                    sni = names.vector(2)? == b"cover.test";
                }
            }
            43 => {
                let mut v = Bytes(v);
                tls13 = v.vector(1)?.chunks_exact(2).any(|x| x == [3, 4]);
            }
            51 => {
                let mut v = Bytes(v);
                let mut shares = Bytes(v.vector(2)?);
                while !shares.0.is_empty() {
                    let group = shares.num(2)?;
                    let bytes = shares.vector(2)?;
                    if group == 29 && bytes.len() == 32 {
                        public = bytes.to_vec();
                    }
                    if group == 0x11ec && bytes.len() == 1216 {
                        hybrid = bytes[1184..].to_vec();
                    }
                }
            }
            _ => {}
        }
    }
    if !sni || !tls13 {
        return Err("REALITY SNI/version".into());
    }
    if public.is_empty() {
        public = hybrid;
    }
    let a = auth_key(&x_shared(private, &public)?, random)?;
    let mut ch0 = ch.to_vec();
    ch0[39..71].fill(0);
    let p = unseal(&a, &random[20..], &ch0, sid)?;
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs();
    if p.len() != 16 || &p[8..] != short || now.abs_diff(length(&p[4..8]) as u64) > 60 {
        return Err("REALITY authentication".into());
    }
    Ok((a, public))
}
struct Stream {
    target: TcpStream,
    consumed: u16,
    send_credit: usize,
}
pub fn serve(
    listener: TcpListener,
    mode: &str,
    certificate: &Path,
    key_file: &Path,
    private: &[u8],
    short: &[u8],
    k: &[u8],
) -> Result<usize> {
    let (mut tcp, _) = listener.accept()?;
    tcp.set_nodelay(true)?;
    tcp.set_read_timeout(Some(std::time::Duration::from_secs(5)))?;
    tcp.set_write_timeout(Some(std::time::Duration::from_secs(5)))?;
    let mut ctx = SslAcceptor::mozilla_intermediate_v5(SslMethod::tls_server())?;
    ctx.set_min_proto_version(Some(SslVersion::TLS1_3))?;
    ctx.set_max_proto_version(Some(SslVersion::TLS1_3))?;
    ctx.set_options(SslOptions::NO_TICKET);
    ctx.set_num_tickets(0)?;
    ctx.set_ciphersuites("TLS_AES_128_GCM_SHA256")?;
    ctx.set_groups_list("X25519")?;
    ctx.set_alpn_select_callback(|_, offered| {
        select_next_proto(b"\x08http/1.1", offered).ok_or(AlpnError::NOACK)
    });
    let saved = if mode == "reality" {
        let (saved, ch) = hello(&mut tcp)?;
        let (a, _) = authentication(&ch, private, short)?;
        let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
        let key = PKey::from_ec_key(EcKey::generate(&group)?)?;
        let mut name = X509NameBuilder::new()?;
        name.append_entry_by_text("CN", "cover.test")?;
        let name = name.build();
        let mut cert = X509::builder()?;
        cert.set_version(2)?;
        cert.set_serial_number(BigNum::from_u32(1)?.to_asn1_integer()?.as_ref())?;
        cert.set_subject_name(&name)?;
        cert.set_issuer_name(&name)?;
        cert.set_pubkey(&key)?;
        cert.set_not_before(Asn1Time::days_from_now(0)?.as_ref())?;
        cert.set_not_after(Asn1Time::days_from_now(1)?.as_ref())?;
        let proof = mac(
            &a,
            &[
                b"Veil-v0.3 server authentication\0".as_slice(),
                &key.public_key_to_der()?,
            ]
            .concat(),
        )?;
        let oid = Asn1Object::from_str("2.5.29.14")?;
        let value = Asn1OctetString::new_from_bytes(&[&[4, 32], proof.as_slice()].concat())?;
        cert.append_extension(X509Extension::new_from_der(&oid, false, &value)?)?;
        cert.sign(&key, MessageDigest::sha256())?;
        ctx.set_certificate(&cert.build())?;
        ctx.set_private_key(&key)?;
        saved
    } else {
        ctx.set_certificate_file(certificate, openssl::ssl::SslFiletype::PEM)?;
        ctx.set_private_key_file(key_file, openssl::ssl::SslFiletype::PEM)?;
        Vec::new()
    };
    ctx.check_private_key()?;
    let mut io = wire::Channel::Tls(ctx.build().accept(wire::Prefixed {
        saved: std::io::Cursor::new(saved),
        stream: tcp,
    })?);
    let b = io.exporter()?;
    let mut auth = [0; 53];
    io.read_exact(&mut auth)?;
    verify_auth(k, &b, &auth)?;
    let mut streams = BTreeMap::<u32, Stream>::new();
    let mut high = 0;
    let mut completed = 0;
    loop {
        let (t, id, p) = match wire::read_frame(&mut io) {
            Ok(f) => f,
            Err(e) => {
                if streams.is_empty() && completed > 0 {
                    return Ok(completed);
                }
                return Err(e);
            }
        };
        if t == 1 {
            if id <= high || streams.len() >= 8 || p.len() != 7 || p[0] != 1 {
                return Err("OPEN format/state in test server".into());
            }
            high = id;
            let address = std::net::SocketAddrV4::new(
                std::net::Ipv4Addr::new(p[1], p[2], p[3], p[4]),
                u16::from_be_bytes([p[5], p[6]]),
            );
            // This fixture intentionally refuses non-loopback targets.
            if !address.ip().is_loopback() {
                return Err("test server only dials loopback".into());
            }
            match crate::socket(address.into()) {
                Ok(target) => {
                    streams.insert(
                        id,
                        Stream {
                            target,
                            consumed: 0,
                            send_credit: 256,
                        },
                    );
                    io.send(&wire::frame(2, id, b""))?;
                }
                Err(_) => io.send(&wire::frame(3, id, &[3]))?,
            }
            continue;
        }
        if id > high {
            return Err("unknown stream".into());
        }
        let Some(s) = streams.get_mut(&id) else {
            continue;
        };
        match t {
            4 => {
                s.target.write_all(&p)?;
                let mut echoed = vec![0; p.len()];
                s.target.read_exact(&mut echoed)?;
                if s.send_credit == 0 {
                    return Err("test fixture response credit exhausted".into());
                }
                s.send_credit -= 1;
                io.send(&wire::frame(4, id, &echoed))?;
                s.consumed += 1;
                if s.consumed == 32 {
                    io.send(&wire::frame(7, id, &32u16.to_be_bytes()))?;
                    s.consumed = 0;
                }
            }
            5 => {
                s.target.shutdown(Shutdown::Write)?;
                let mut byte = [0; 1];
                if s.target.read(&mut byte)? != 0 {
                    return Err("target trailing bytes".into());
                }
                io.send(&[wire::frame(5, id, b""), wire::frame(8, id, b"")].concat())?;
                streams.remove(&id);
                completed += 1;
            }
            6 => {
                streams.remove(&id);
            }
            7 => {
                let n = length(&p);
                if n == 0 || n > 256 - s.send_credit {
                    return Err("bad client credit".into());
                }
                s.send_credit += n;
            }
            _ => return Err("unexpected client frame".into()),
        }
    }
}
