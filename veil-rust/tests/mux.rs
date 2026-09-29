use std::{io, time::Duration};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use veil_rust::{
    config::Traffic,
    mux::Session,
    wire::{self, Target},
};

fn start(
    io: tokio::io::DuplexStream,
    server: bool,
) -> (
    Session,
    tokio::sync::mpsc::Receiver<(veil_rust::mux::Stream, Vec<u8>)>,
) {
    Session::start(
        io,
        server,
        vec![],
        Traffic::default(),
        Duration::from_secs(120),
        Duration::from_secs(60),
    )
    .unwrap()
}
async fn deadline<F: std::future::Future>(f: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(10), f)
        .await
        .expect("test hung")
}

#[tokio::test]
async fn duplex_half_close_large_credit_and_reuse() {
    deadline(async {
        let (a, b) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let (_server, mut incoming) = start(b, true);
        let server = tokio::spawn(async move {
            for _ in 0..3 {
                let (mut st, _) = incoming.recv().await.unwrap();
                st.accept().unwrap();
                let mut payload = Vec::new();
                st.read_to_end(&mut payload).await.unwrap();
                // Response is sent only after client FIN. Whole-socket close fails this.
                st.write_all(&payload).await.unwrap();
                st.shutdown().await.unwrap();
                st.finish().await.unwrap();
            }
        });
        for n in [37, 10 << 20, 17] {
            let mut st = client
                .reserve(&Target::from(
                    "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
                ))
                .unwrap();
            st.ready().await.unwrap();
            let payload: Vec<u8> = (0..n).map(|i| (i % 251) as u8).collect();
            st.write_all(&payload).await.unwrap();
            st.shutdown().await.unwrap();
            let mut got = Vec::new();
            st.read_to_end(&mut got).await.unwrap();
            assert_eq!(got, payload);
            st.finish().await.unwrap();
        }
        server.await.unwrap();
    })
    .await
}

#[tokio::test]
async fn blocked_stream_does_not_block_other_streams() {
    deadline(async {
        let (a, b) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let (_server, mut incoming) = start(b, true);
        let target = Target::from("127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap());
        let mut blocked = client.reserve(&target).unwrap();
        let (mut sink, _) = incoming.recv().await.unwrap();
        sink.accept().unwrap();
        blocked.ready().await.unwrap();
        let sender = tokio::spawn(async move { blocked.write_all(&vec![3; 20 << 20]).await });
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(!sender.is_finished()); // No reads from sink, exhaust 256 frame credit.
        let mut good = client.reserve(&target).unwrap();
        let (mut peer, _) = incoming.recv().await.unwrap();
        peer.accept().unwrap();
        good.ready().await.unwrap();
        good.write_all(b"independent").await.unwrap();
        good.flush().await.unwrap();
        let mut got = [0; 11];
        peer.read_exact(&mut got).await.unwrap();
        assert_eq!(&got, b"independent");
        drop(sink);
        assert!(sender.await.unwrap().is_err());
    })
    .await
}

#[tokio::test]
async fn partial_payload_is_visible_before_whole_frame() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let (_server, mut incoming) = start(a, true);
        let addr = Target::from("127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap())
            .encode()
            .unwrap();
        let mut bytes = vec![];
        wire::frame(&mut bytes, wire::OPEN, 1, &addr);
        peer.write_all(&bytes).await.unwrap();
        let (mut st, _) = incoming.recv().await.unwrap();
        st.accept().unwrap();
        let mut opened = [0; 8];
        peer.read_exact(&mut opened).await.unwrap();
        bytes.clear();
        wire::frame(&mut bytes, wire::DATA, 1, &[9; 32768]);
        peer.write_all(&bytes[..25]).await.unwrap();
        let mut got = [0; 17];
        st.read_exact(&mut got).await.unwrap();
        assert_eq!(got, [9; 17]);
        // Truncation must become an error, not normal EOF.
        drop(peer);
        assert!(st.read_u8().await.is_err());
    })
    .await
}

#[tokio::test]
async fn malformed_state_fails_pending_streams() {
    for (kind, id, payload) in [
        (wire::DONE, 1, vec![]),
        (wire::CREDIT, 1, vec![0, 0]),
        (wire::CREDIT, 1, vec![0, 1]),
        (wire::FIN, 1, vec![]),
        (wire::DATA, 3, vec![1]),
        (99, 1, vec![]),
        (wire::OPENED, 2, vec![]),
    ] {
        deadline(async {
            let (a, mut peer) = tokio::io::duplex(65536);
            let (client, _) = start(a, false);
            let mut st = client
                .reserve(&Target::from(
                    "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
                ))
                .unwrap();
            let mut bytes = vec![];
            wire::frame(&mut bytes, kind, id, &payload);
            peer.write_all(&bytes).await.unwrap();
            assert!(st.ready().await.is_err());
        })
        .await;
    }
}

#[tokio::test]
async fn failed_reset_and_late_frames_allow_reuse() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let target = Target::from("127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap());
        let mut failed = client.reserve(&target).unwrap();
        let mut buf = vec![];
        wire::frame(&mut buf, wire::FAILED, 1, &[3]);
        peer.write_all(&buf).await.unwrap();
        assert_eq!(
            failed.ready().await.unwrap_err().kind(),
            io::ErrorKind::ConnectionRefused
        );
        drop(failed);
        let mut good = client.reserve(&target).unwrap();
        buf.clear();
        wire::frame(&mut buf, wire::DATA, 1, b"late");
        wire::frame(&mut buf, wire::OPENED, 3, &[]);
        peer.write_all(&buf).await.unwrap();
        good.ready().await.unwrap();
        buf.clear();
        wire::frame(&mut buf, wire::RESET, 3, &[]);
        peer.write_all(&buf).await.unwrap();
        assert!(good.read_u8().await.is_err());
        let mut last = client.reserve(&target).unwrap();
        buf.clear();
        wire::frame(&mut buf, wire::OPENED, 5, &[]);
        peer.write_all(&buf).await.unwrap();
        last.ready().await.unwrap();
    })
    .await
}

#[tokio::test]
async fn physical_eof_is_not_fin() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let mut st = client
            .reserve(&Target::from(
                "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
            ))
            .unwrap();
        let mut b = vec![];
        wire::frame(&mut b, wire::OPENED, 1, &[]);
        peer.write_all(&b).await.unwrap();
        st.ready().await.unwrap();
        drop(peer);
        let mut b = [0; 1];
        assert!(st.read(&mut b).await.is_err());
        assert!(st.finish().await.is_err());
    })
    .await
}

#[tokio::test]
async fn server_fin_does_not_close_upload() {
    deadline(async {
        let (a, b) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let (_server, mut incoming) = start(b, true);
        let mut c = client
            .reserve(&Target::from(
                "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
            ))
            .unwrap();
        let (mut s, _) = incoming.recv().await.unwrap();
        s.accept().unwrap();
        c.ready().await.unwrap();
        s.write_all(b"response first").await.unwrap();
        s.shutdown().await.unwrap();
        let mut reply = Vec::new();
        c.read_to_end(&mut reply).await.unwrap();
        assert_eq!(reply, b"response first");
        c.write_all(b"upload after peer FIN").await.unwrap();
        c.shutdown().await.unwrap();
        let mut request = Vec::new();
        s.read_to_end(&mut request).await.unwrap();
        assert_eq!(request, b"upload after peer FIN");
        s.finish().await.unwrap();
        c.finish().await.unwrap();
    })
    .await
}

#[tokio::test]
async fn receive_window_overrun_is_fatal() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let (_server, mut incoming) = start(a, true);
        let addr = Target::from("127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap())
            .encode()
            .unwrap();
        let mut bytes = vec![];
        wire::frame(&mut bytes, wire::OPEN, 1, &addr);
        peer.write_all(&bytes).await.unwrap();
        let (mut st, _) = incoming.recv().await.unwrap();
        st.accept().unwrap();
        let mut opened = [0; 8];
        peer.read_exact(&mut opened).await.unwrap();
        bytes.clear();
        for _ in 0..257 {
            wire::frame(&mut bytes, wire::DATA, 1, &[1]);
        }
        peer.write_all(&bytes).await.unwrap();
        st.cancelled().await;
        assert!(st.read_u8().await.is_err());
    })
    .await
}

#[tokio::test]
async fn both_fin_without_done_is_not_success() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let (client, _) = start(a, false);
        let mut st = client
            .reserve(&Target::from(
                "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
            ))
            .unwrap();
        let mut b = vec![];
        wire::frame(&mut b, wire::OPENED, 1, &[]);
        peer.write_all(&b).await.unwrap();
        st.ready().await.unwrap();
        st.shutdown().await.unwrap();
        b.clear();
        wire::frame(&mut b, wire::FIN, 1, &[]);
        peer.write_all(&b).await.unwrap();
        let mut b = [0; 1];
        assert_eq!(st.read(&mut b).await.unwrap(), 0);
        drop(peer);
        assert!(st.finish().await.is_err());
    })
    .await
}

#[tokio::test]
async fn idle_wakeups_do_not_consume_startup_shape() {
    deadline(async {
        let (a, mut peer) = tokio::io::duplex(65536);
        let profile = Traffic {
            startup_bytes: veil_rust::config::Range { min: 128, max: 128 },
            startup_writes: veil_rust::config::Range { min: 3, max: 3 },
            ..Traffic::default()
        };
        let (client, _) = Session::start(
            a,
            false,
            vec![],
            profile,
            Duration::from_secs(120),
            Duration::from_secs(60),
        )
        .unwrap();
        let mut st = client
            .reserve(&Target::from(
                "127.0.0.1:443".parse::<std::net::SocketAddr>().unwrap(),
            ))
            .unwrap();
        let mut open = [0; 15];
        peer.read_exact(&mut open).await.unwrap();
        let mut b = vec![];
        wire::frame(&mut b, wire::OPENED, 1, &[]);
        peer.write_all(&b).await.unwrap();
        st.ready().await.unwrap();
        tokio::time::sleep(Duration::from_millis(20)).await;
        st.write_all(&[7; 512]).await.unwrap();
        st.flush().await.unwrap();
        let mut h = [0; 8];
        peer.read_exact(&mut h).await.unwrap();
        assert_eq!(wire::header(h).unwrap(), (wire::DATA, 1, 120));
    })
    .await
}
