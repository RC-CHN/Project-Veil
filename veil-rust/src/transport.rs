use crate::{config::Tls, wire};
use rustls::{
    ClientConfig, RootCertStore, ServerConfig,
    crypto::ring,
    pki_types::{CertificateDer, PrivateKeyDer, ServerName, pem::PemObject},
};
use std::{fs, io, sync::Arc, time::Duration};
use tokio::{io::AsyncReadExt, net::TcpStream};
use tokio_rustls::{TlsAcceptor, TlsConnector, TlsStream};

pub const HANDSHAKE: Duration = Duration::from_secs(10);
fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    let mut p = ring::default_provider();
    p.cipher_suites = vec![ring::cipher_suite::TLS13_AES_128_GCM_SHA256];
    p.kx_groups = vec![ring::kx_group::X25519];
    Arc::new(p)
}
pub fn client(t: &Tls) -> io::Result<TlsConnector> {
    if !t.ca_file.is_empty() && !t.ca_pem.is_empty() {
        return Err(io::Error::other("choose ca_file or ca_pem"));
    }
    let pem = if !t.ca_pem.is_empty() {
        t.ca_pem.as_bytes().to_vec()
    } else if !t.ca_file.is_empty() {
        fs::read(&t.ca_file)?
    } else {
        return Err(io::Error::other("explicit ca_file or ca_pem required"));
    };
    let mut roots = RootCertStore::empty();
    for cert in CertificateDer::pem_slice_iter(&pem) {
        roots
            .add(cert.map_err(io::Error::other)?)
            .map_err(io::Error::other)?;
    }
    if roots.is_empty() {
        return Err(io::Error::other("no CA certificates"));
    }
    let mut c = ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(io::Error::other)?
        .with_root_certificates(roots)
        .with_no_client_auth();
    c.alpn_protocols = vec![b"http/1.1".to_vec()];
    c.resumption = rustls::client::Resumption::disabled();
    Ok(TlsConnector::from(Arc::new(c)))
}
pub fn server(t: &Tls) -> io::Result<TlsAcceptor> {
    let certs = CertificateDer::pem_file_iter(&t.certificate)
        .map_err(io::Error::other)?
        .collect::<Result<Vec<_>, _>>()
        .map_err(io::Error::other)?;
    let key = PrivateKeyDer::from_pem_file(&t.private_key_file).map_err(io::Error::other)?;
    let mut c = ServerConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(io::Error::other)?
        .with_no_client_auth()
        .with_single_cert(certs, key)
        .map_err(io::Error::other)?;
    c.alpn_protocols = vec![b"http/1.1".to_vec()];
    c.send_tls13_tickets = 0;
    Ok(TlsAcceptor::from(Arc::new(c)))
}
pub async fn connect(
    connector: &TlsConnector,
    address: &str,
    name: &str,
    key: &[u8; 32],
) -> io::Result<(TlsStream<TcpStream>, Vec<u8>)> {
    tokio::time::timeout(HANDSHAKE, async {
        let raw = TcpStream::connect(address).await?;
        raw.set_nodelay(true)?;
        let name = ServerName::try_from(name.to_owned()).map_err(io::Error::other)?;
        let c = connector.connect(name, raw).await?;
        let b = c
            .get_ref()
            .1
            .export_keying_material([0; 32], b"EXPORTER-Veil-v0.3", Some(&[]))
            .map_err(io::Error::other)?;
        Ok((TlsStream::Client(c), wire::auth(key, &b)?.to_vec()))
    })
    .await?
}
pub async fn accept(
    acceptor: &TlsAcceptor,
    raw: TcpStream,
    key: &[u8; 32],
) -> io::Result<TlsStream<TcpStream>> {
    tokio::time::timeout(HANDSHAKE, async {
        raw.set_nodelay(true)?;
        let mut c = acceptor.accept(raw).await?;
        let b = c
            .get_ref()
            .1
            .export_keying_material([0; 32], b"EXPORTER-Veil-v0.3", Some(&[]))
            .map_err(io::Error::other)?;
        let mut a = [0; 53];
        c.read_exact(&mut a[..5]).await?;
        if a[..5] != [1, 0, 0, 49, 3] {
            return Err(wire::invalid());
        }
        c.read_exact(&mut a[5..]).await?;
        wire::verify_auth(key, &b, &a)?;
        Ok(TlsStream::Server(c))
    })
    .await?
}
