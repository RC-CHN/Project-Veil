use crate::Result;
use openssl::{
    derive::Deriver,
    hash::MessageDigest,
    pkey::{Id, PKey},
    rand::rand_bytes,
    sha::sha256,
    sign::Signer,
    symm::{Cipher, decrypt_aead, encrypt_aead},
};

pub fn random(n: usize) -> Result<Vec<u8>> {
    let mut b = vec![0; n];
    rand_bytes(&mut b)?;
    Ok(b)
}
pub fn hmac(md: MessageDigest, key: &[u8], data: &[u8]) -> Result<Vec<u8>> {
    let key = PKey::hmac(key)?;
    let mut signer = Signer::new(md, &key)?;
    signer.update(data)?;
    Ok(signer.sign_to_vec()?)
}
pub fn mac(key: &[u8], data: &[u8]) -> Result<Vec<u8>> {
    hmac(MessageDigest::sha256(), key, data)
}
pub fn expand(key: &[u8], info: &[u8], len: usize) -> Result<Vec<u8>> {
    if len > 255 * 32 {
        return Err("HKDF length".into());
    }
    let mut result = Vec::new();
    let mut last = Vec::new();
    for i in 1..=len.div_ceil(32) {
        last = mac(key, &[last.as_slice(), info, &[i as u8]].concat())?;
        result.extend_from_slice(&last);
    }
    result.truncate(len);
    Ok(result)
}
pub fn label(key: &[u8], name: &str, context: &[u8], len: usize) -> Result<Vec<u8>> {
    let name = [b"tls13 ".as_slice(), name.as_bytes()].concat();
    let info = [
        (len as u16).to_be_bytes().as_slice(),
        &[name.len() as u8],
        &name,
        &[context.len() as u8],
        context,
    ]
    .concat();
    expand(key, &info, len)
}
pub fn derive(key: &[u8], name: &str, transcript: &[u8]) -> Result<Vec<u8>> {
    label(key, name, &sha256(transcript), 32)
}
pub fn exporter(master: &[u8]) -> Result<Vec<u8>> {
    let secret = label(master, "EXPORTER-Veil-v0.3", &sha256(b""), 32)?;
    label(&secret, "exporter", &sha256(b""), 32)
}
pub fn x_public(private: &[u8]) -> Result<Vec<u8>> {
    Ok(PKey::private_key_from_raw_bytes(private, Id::X25519)?.raw_public_key()?)
}
pub fn x_shared(private: &[u8], public: &[u8]) -> Result<Vec<u8>> {
    let key = PKey::private_key_from_raw_bytes(private, Id::X25519)?;
    let peer = PKey::public_key_from_raw_bytes(public, Id::X25519)?;
    let mut d = Deriver::new(&key)?;
    d.set_peer(&peer)?;
    let z = d.derive_to_vec()?;
    if z.iter().all(|x| *x == 0) {
        return Err("zero X25519 secret".into());
    }
    Ok(z)
}
pub fn auth_key(z: &[u8], random: &[u8]) -> Result<Vec<u8>> {
    expand(&mac(&random[..20], z)?, b"REALITY", 32)
}
pub fn seal(key: &[u8], nonce: &[u8], aad: &[u8], p: &[u8]) -> Result<Vec<u8>> {
    let cipher = if key.len() == 32 {
        Cipher::aes_256_gcm()
    } else {
        Cipher::aes_128_gcm()
    };
    let mut tag = [0; 16];
    let mut b = encrypt_aead(cipher, key, Some(nonce), aad, p, &mut tag)?;
    b.extend_from_slice(&tag);
    Ok(b)
}
pub fn unseal(key: &[u8], nonce: &[u8], aad: &[u8], b: &[u8]) -> Result<Vec<u8>> {
    if b.len() < 16 {
        return Err("short AEAD ciphertext".into());
    }
    let cipher = if key.len() == 32 {
        Cipher::aes_256_gcm()
    } else {
        Cipher::aes_128_gcm()
    };
    Ok(decrypt_aead(
        cipher,
        key,
        Some(nonce),
        aad,
        &b[..b.len() - 16],
        &b[b.len() - 16..],
    )?)
}
pub fn same(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && openssl::memcmp::eq(a, b)
}
pub fn proof(k: &[u8], b: &[u8], nonce: &[u8]) -> Result<Vec<u8>> {
    if k.len() != 32 || b.len() != 32 || nonce.len() != 16 {
        return Err("AUTH input size".into());
    }
    let body = [&[3], nonce].concat();
    let tag = mac(
        k,
        &[b"Veil-v0.3 client authentication\0".as_slice(), b, &body].concat(),
    )?;
    Ok([&[1, 0, 0, 49], body.as_slice(), &tag].concat())
}
pub fn verify_auth(k: &[u8], b: &[u8], p: &[u8]) -> Result<()> {
    if p.len() != 53 || p[..5] != [1, 0, 0, 49, 3] || !same(p, &proof(k, b, &p[5..21])?) {
        return Err("AUTH rejected".into());
    }
    Ok(())
}
pub fn u24(n: usize) -> [u8; 3] {
    [(n >> 16) as u8, (n >> 8) as u8, n as u8]
}
pub fn length(b: &[u8]) -> usize {
    b.iter().fold(0, |n, x| (n << 8) | (*x as usize))
}
pub fn handshake(typ: u8, body: &[u8]) -> Vec<u8> {
    [&[typ], u24(body.len()).as_slice(), body].concat()
}
pub fn vector16(b: &[u8]) -> Vec<u8> {
    [(b.len() as u16).to_be_bytes().as_slice(), b].concat()
}
pub fn ext(typ: u16, b: &[u8]) -> Vec<u8> {
    [typ.to_be_bytes().as_slice(), &vector16(b)].concat()
}
// Checked parsing is shared only within this independent test executable.
pub struct Bytes<'a>(pub &'a [u8]);
impl<'a> Bytes<'a> {
    pub fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        if n > self.0.len() {
            return Err("truncated structure".into());
        }
        let (a, b) = self.0.split_at(n);
        self.0 = b;
        Ok(a)
    }
    pub fn num(&mut self, n: usize) -> Result<usize> {
        Ok(length(self.take(n)?))
    }
    pub fn vector(&mut self, n: usize) -> Result<&'a [u8]> {
        let n = self.num(n)?;
        self.take(n)
    }
}
