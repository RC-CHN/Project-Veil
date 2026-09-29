use crate::{config::Traffic, wire::*};
use bytes::{Buf, Bytes, BytesMut};
use std::{
    collections::{BTreeMap, VecDeque},
    future::poll_fn,
    io,
    pin::Pin,
    sync::{Arc, Mutex, MutexGuard},
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, ReadBuf},
    sync::{Notify, mpsc},
};

const BATCH: usize = 128 << 10;
const SEND_BUFFER: usize = 256 << 10;

type Terminal = Option<Result<(), io::ErrorKind>>;
struct Chunk {
    bytes: BytesMut,
    complete: bool,
}
struct Slot {
    opened: bool,
    local_fin: bool,
    fin_sent: bool,
    fin_flushed: bool,
    remote_fin: bool,
    terminal: Terminal,
    transferred: usize,
    send_credit: u16,
    recv_credit: u16,
    consumed: u16,
    credit_at: u16,
    input: VecDeque<Chunk>,
    output: VecDeque<Bytes>,
    queued: usize,
    inflight: usize,
    read_waker: Option<Waker>,
    write_waker: Option<Waker>,
    ready_waker: Option<Waker>,
    done_waker: Option<Waker>,
    activity: Instant,
}
impl Slot {
    fn new(credit_at: u16) -> Self {
        Self {
            opened: false,
            local_fin: false,
            fin_sent: false,
            fin_flushed: false,
            remote_fin: false,
            terminal: None,
            transferred: 0,
            send_credit: WINDOW,
            recv_credit: WINDOW,
            consumed: 0,
            credit_at,
            input: VecDeque::new(),
            output: VecDeque::new(),
            queued: 0,
            inflight: 0,
            read_waker: None,
            write_waker: None,
            ready_waker: None,
            done_waker: None,
            activity: Instant::now(),
        }
    }
    fn wake(&mut self) {
        for w in [
            &mut self.read_waker,
            &mut self.write_waker,
            &mut self.ready_waker,
            &mut self.done_waker,
        ] {
            if let Some(w) = w.take() {
                w.wake();
            }
        }
    }
    fn fail(&mut self, kind: io::ErrorKind) {
        if self.terminal == Some(Ok(())) {
            return;
        }
        self.terminal = Some(Err(kind));
        self.input.clear();
        self.output.clear();
        self.queued = 0;
        self.wake();
    }
    fn check(&self) -> io::Result<()> {
        match self.terminal {
            Some(Err(e)) => Err(io::Error::new(e, "Veil stream failed")),
            _ => Ok(()),
        }
    }
}
struct Control {
    kind: u8,
    id: u32,
    payload: Vec<u8>,
}
struct State {
    streams: BTreeMap<u32, Slot>,
    high: u32,
    last_sent: u32,
    controls: VecDeque<Control>,
    failure: Option<io::ErrorKind>,
    prefix: Vec<u8>,
    startup: usize,
    idle_since: Instant,
}
impl State {
    fn active(&self) -> usize {
        self.streams
            .values()
            .filter(|s| s.terminal.is_none())
            .count()
    }
    fn control(&mut self, kind: u8, id: u32, payload: &[u8]) -> io::Result<()> {
        if kind == CREDIT
            && let Some(c) = self
                .controls
                .iter_mut()
                .find(|c| c.kind == CREDIT && c.id == id)
        {
            let amount = u16::from_be_bytes(c.payload[..].try_into().unwrap())
                + u16::from_be_bytes(payload.try_into().unwrap());
            if amount > WINDOW {
                return Err(invalid());
            }
            c.payload.copy_from_slice(&amount.to_be_bytes());
            return Ok(());
        }
        if self.controls.len() >= 64 {
            return Err(io::Error::other("Veil control queue full"));
        }
        self.controls.push_back(Control {
            kind,
            id,
            payload: payload.to_vec(),
        });
        Ok(())
    }
}
struct Inner {
    state: Mutex<State>,
    wake: Notify,
    server: bool,
    traffic: Traffic,
}
impl Inner {
    fn lock(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap()
    }
    fn fail(&self, kind: io::ErrorKind) {
        let mut s = self.lock();
        if s.failure.is_some() {
            return;
        }
        s.failure = Some(kind);
        for slot in s.streams.values_mut() {
            slot.fail(kind);
        }
        s.controls.clear();
        self.wake.notify_one();
    }
}
struct Lease(Arc<Inner>);
impl Drop for Lease {
    fn drop(&mut self) {
        self.0.fail(io::ErrorKind::ConnectionAborted);
    }
}
#[derive(Clone)]
pub struct Session(Arc<Lease>);
pub type Incoming = mpsc::Receiver<(Stream, Vec<u8>)>;

pub struct Stream {
    inner: Arc<Inner>,
    id: u32,
}

impl Session {
    /// AUTH must already be checked for a server. A client supplies its AUTH
    /// prefix so the first OPEN is coalesced into the same application write.
    pub fn start<T>(
        io: T,
        server: bool,
        prefix: Vec<u8>,
        mut traffic: Traffic,
        idle: Duration,
        pool_idle: Duration,
    ) -> io::Result<(Self, Incoming)>
    where
        T: AsyncRead + AsyncWrite + Unpin + Send + 'static,
    {
        traffic.validate()?;
        traffic.quantum_bytes = traffic.quantum_bytes.narrowed();
        traffic.credit_blocks = traffic.credit_blocks.narrowed();
        traffic.startup_bytes = traffic.startup_bytes.narrowed();
        traffic.startup_writes = traffic.startup_writes.narrowed();
        let inner = Arc::new(Inner {
            state: Mutex::new(State {
                streams: BTreeMap::new(),
                high: 0,
                last_sent: 0,
                controls: VecDeque::new(),
                failure: None,
                prefix,
                startup: 0,
                idle_since: Instant::now(),
            }),
            wake: Notify::new(),
            server,
            traffic,
        });
        let (tx, rx) = mpsc::channel(MAX_STREAMS);
        let bg = inner.clone();
        tokio::spawn(async move {
            let (r, w) = tokio::io::split(io);
            let result = tokio::try_join!(
                read_loop(r, &bg, tx),
                write_loop(w, &bg, idle),
                watchdog(&bg, idle, pool_idle)
            );
            bg.fail(
                result
                    .err()
                    .map_or(io::ErrorKind::ConnectionAborted, |e| e.kind()),
            );
        });
        Ok((Self(Arc::new(Lease(inner))), rx))
    }
    fn inner(&self) -> &Arc<Inner> {
        &self.0.0
    }
    pub fn snapshot(&self) -> (usize, bool) {
        let s = self.inner().lock();
        (s.active(), s.failure.is_some() || s.high >= u32::MAX - 2)
    }
    pub fn bulk(&self) -> bool {
        self.inner()
            .lock()
            .streams
            .values()
            .any(|s| s.terminal.is_none() && s.transferred >= 256 << 10)
    }
    pub fn close(&self) {
        self.inner().fail(io::ErrorKind::ConnectionAborted);
    }
    /// Reservation is synchronous: concurrent callers cannot overbook a session.
    pub fn reserve(&self, target: &Target) -> io::Result<Stream> {
        let payload = target.encode()?;
        let inner = self.inner();
        let mut s = inner.lock();
        if inner.server || s.failure.is_some() {
            return Err(io::ErrorKind::NotConnected.into());
        }
        if s.active() >= MAX_STREAMS || s.high >= u32::MAX - 2 {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let id = if s.high == 0 { 1 } else { s.high + 2 };
        s.control(OPEN, id, &payload)?;
        s.high = id;
        s.streams
            .insert(id, Slot::new(inner.traffic.credit_blocks.sample() as u16));
        inner.wake.notify_one();
        Ok(Stream {
            inner: inner.clone(),
            id,
        })
    }
}
impl Stream {
    pub async fn ready(&mut self) -> io::Result<()> {
        poll_fn(|cx| {
            let mut s = self.inner.lock();
            let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
            st.check()?;
            if st.opened {
                Poll::Ready(Ok(()))
            } else {
                st.ready_waker = Some(cx.waker().clone());
                Poll::Pending
            }
        })
        .await
    }
    /// Called only after the target TCP dial actually succeeded.
    pub fn accept(&mut self) -> io::Result<()> {
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        st.check()?;
        if !self.inner.server || st.opened {
            return Err(invalid());
        }
        st.opened = true;
        s.control(OPENED, self.id, &[])?;
        self.inner.wake.notify_one();
        Ok(())
    }
    pub fn reject(&mut self, code: u8) -> io::Result<()> {
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        if !self.inner.server || st.opened {
            return Err(invalid());
        }
        st.fail(io::ErrorKind::ConnectionRefused);
        s.control(FAILED, self.id, &[code])?;
        self.inner.wake.notify_one();
        Ok(())
    }
    pub async fn cancelled(&self) {
        poll_fn(|cx| {
            let mut s = self.inner.lock();
            let Some(st) = s.streams.get_mut(&self.id) else {
                return Poll::Ready(());
            };
            if st.terminal.is_some() {
                Poll::Ready(())
            } else {
                st.done_waker = Some(cx.waker().clone());
                Poll::Pending
            }
        })
        .await
    }
    /// After both local relay pumps finish, the server sends DONE; a client
    /// waits for DONE before treating the transfer as successfully completed.
    pub async fn finish(&mut self) -> io::Result<()> {
        if self.inner.server {
            let mut s = self.inner.lock();
            let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
            st.check()?;
            if !st.fin_flushed || !st.remote_fin || !st.input.is_empty() {
                return Err(invalid());
            }
            s.control(DONE, self.id, &[])?;
            self.inner.wake.notify_one();
        }
        poll_fn(|cx| {
            let mut s = self.inner.lock();
            let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
            st.check()?;
            if st.terminal == Some(Ok(())) {
                Poll::Ready(Ok(()))
            } else {
                st.done_waker = Some(cx.waker().clone());
                Poll::Pending
            }
        })
        .await
    }
}
impl Drop for Stream {
    fn drop(&mut self) {
        let mut s = self.inner.lock();
        if let Some(st) = s.streams.remove(&self.id)
            && st.terminal.is_none()
            && s.failure.is_none()
            && s.control(RESET, self.id, &[]).is_err()
        {
            drop(s);
            self.inner.fail(io::ErrorKind::ConnectionAborted);
            return;
        }
        if s.active() == 0 {
            s.idle_since = Instant::now();
        }
        self.inner.wake.notify_one();
    }
}
impl AsyncRead for Stream {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        out: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        if out.remaining() == 0 {
            return Poll::Ready(Ok(()));
        }
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        st.check()?;
        let before = out.filled().len();
        while let Some(c) = st.input.front_mut() {
            let n = out.remaining().min(c.bytes.len());
            out.put_slice(&c.bytes.split_to(n));
            if !c.bytes.is_empty() || !c.complete {
                break;
            }
            st.input.pop_front();
            st.consumed += 1;
            if out.remaining() == 0 {
                break;
            }
        }
        let amount = if st.consumed >= st.credit_at {
            std::mem::take(&mut st.consumed)
        } else {
            0
        };
        st.recv_credit += amount;
        let readable = out.filled().len() != before || st.remote_fin && st.input.is_empty();
        if !readable {
            st.read_waker = Some(cx.waker().clone());
        }
        if amount > 0 {
            s.control(CREDIT, self.id, &amount.to_be_bytes())?;
            self.inner.wake.notify_one();
        }
        if readable {
            Poll::Ready(Ok(()))
        } else {
            Poll::Pending
        }
    }
}
impl AsyncWrite for Stream {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        st.check()?;
        if !st.opened || st.local_fin {
            return Poll::Ready(Err(io::ErrorKind::BrokenPipe.into()));
        }
        if buf.is_empty() {
            return Poll::Ready(Ok(0));
        }
        let n = buf.len().min(SEND_BUFFER - st.queued).min(BATCH - 32);
        if n == 0 {
            st.write_waker = Some(cx.waker().clone());
            return Poll::Pending;
        }
        st.output.push_back(Bytes::copy_from_slice(&buf[..n]));
        st.queued += n;
        self.inner.wake.notify_one();
        Poll::Ready(Ok(n))
    }
    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        st.check()?;
        if st.queued == 0 && st.inflight == 0 {
            Poll::Ready(Ok(()))
        } else {
            st.write_waker = Some(cx.waker().clone());
            Poll::Pending
        }
    }
    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        let mut s = self.inner.lock();
        let st = s.streams.get_mut(&self.id).ok_or_else(invalid)?;
        st.check()?;
        st.local_fin = true;
        self.inner.wake.notify_one();
        if st.fin_flushed {
            Poll::Ready(Ok(()))
        } else {
            st.write_waker = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

async fn read_loop<R: AsyncRead + Unpin>(
    mut r: R,
    inner: &Arc<Inner>,
    accepted: mpsc::Sender<(Stream, Vec<u8>)>,
) -> io::Result<()> {
    // Buffering reduces per-header TLS calls without delaying partial DATA.
    let mut r = tokio::io::BufReader::with_capacity(BATCH, &mut r);
    let mut scratch = vec![0; BLOCK];
    loop {
        let mut h = [0; 8];
        r.read_exact(&mut h).await?;
        let (kind, id, n) = header(h)?;
        {
            let mut s = inner.lock();
            if kind == OPEN {
                if !inner.server || id <= s.high {
                    return Err(invalid());
                }
            } else if id > s.high {
                return Err(invalid());
            }
            if kind == DATA
                && let Some(st) = s.streams.get_mut(&id).filter(|s| s.terminal.is_none())
            {
                if !st.opened || st.remote_fin || st.recv_credit == 0 {
                    return Err(invalid());
                }
                st.recv_credit -= 1;
                st.input.push_back(Chunk {
                    bytes: BytesMut::with_capacity(n),
                    complete: false,
                });
            }
        }
        if kind == DATA {
            let mut left = n;
            while left > 0 {
                let got = r.read(&mut scratch[..left]).await?;
                if got == 0 {
                    return Err(io::ErrorKind::UnexpectedEof.into());
                }
                left -= got;
                let mut s = inner.lock();
                if let Some(st) = s.streams.get_mut(&id).filter(|s| s.terminal.is_none()) {
                    let c = st.input.back_mut().ok_or_else(invalid)?;
                    c.bytes.extend_from_slice(&scratch[..got]);
                    c.complete = left == 0;
                    st.activity = Instant::now();
                    st.transferred += got;
                    if let Some(w) = st.read_waker.take() {
                        w.wake();
                    }
                }
            }
            continue;
        }
        r.read_exact(&mut scratch[..n]).await?;
        let p = &scratch[..n];
        let mut s = inner.lock();
        if kind == OPEN {
            s.high = id;
            if s.active() >= MAX_STREAMS || accepted.capacity() == 0 {
                s.control(FAILED, id, &[1])?;
            } else {
                s.streams
                    .insert(id, Slot::new(inner.traffic.credit_blocks.sample() as u16));
                // Capacity was checked and this is the sole producer. Avoid
                // dropping a Stream while holding its session lock on error.
                let permit = accepted
                    .try_reserve()
                    .map_err(|_| io::ErrorKind::ConnectionAborted)?;
                permit.send((
                    Stream {
                        inner: inner.clone(),
                        id,
                    },
                    p.to_vec(),
                ));
            }
        } else if let Some(st) = s.streams.get_mut(&id).filter(|s| s.terminal.is_none()) {
            match kind {
                OPENED if !inner.server && !st.opened => {
                    st.opened = true;
                    st.wake();
                }
                FAILED if !inner.server && !st.opened => st.fail(match p[0] {
                    3 => io::ErrorKind::ConnectionRefused,
                    4 => io::ErrorKind::TimedOut,
                    _ => io::ErrorKind::ConnectionAborted,
                }),
                FIN if st.opened && !st.remote_fin => {
                    st.remote_fin = true;
                    st.wake();
                }
                RESET => st.fail(io::ErrorKind::ConnectionReset),
                DONE if !inner.server && st.fin_sent && st.remote_fin => {
                    st.terminal = Some(Ok(()));
                    st.wake();
                }
                CREDIT => {
                    let k = u16::from_be_bytes(p.try_into().unwrap());
                    if k == 0 || k > WINDOW - st.send_credit {
                        return Err(invalid());
                    }
                    st.send_credit += k;
                }
                _ => return Err(invalid()),
            }
        }
        inner.wake.notify_one();
    }
}

fn batch(inner: &Inner, out: &mut Vec<u8>) -> io::Result<Vec<(u32, bool, bool)>> {
    let mut s = inner.lock();
    if let Some(e) = s.failure {
        return Err(e.into());
    }
    let ready = s
        .streams
        .values()
        .filter(|st| {
            st.terminal.is_none() && st.opened && st.send_credit > 0 && !st.output.is_empty()
        })
        .count();
    let needs_fin = s.streams.values().any(|st| {
        st.terminal.is_none() && st.opened && st.local_fin && !st.fin_sent && st.output.is_empty()
    });
    // Empty wakeups must not consume startup shaping slots.
    if s.controls.is_empty() && ready == 0 && !needs_fin {
        return Ok(Vec::new());
    }
    let active = s.active();
    if active > 1 {
        s.startup = 0;
    }
    if s.controls
        .front()
        .is_some_and(|c| matches!(c.kind, OPEN | OPENED))
        && active == 1
    {
        s.startup = inner.traffic.startup_writes.sample();
    }
    let limit = if s.startup > 0 {
        s.startup -= 1;
        inner.traffic.startup_bytes.sample()
    } else {
        BATCH
    };
    let mut touched = Vec::with_capacity(MAX_STREAMS);
    if !s.controls.is_empty() {
        out.append(&mut s.prefix);
    }
    while let Some(c) = s.controls.pop_front() {
        frame(out, c.kind, c.id, &c.payload);
        if c.kind == DONE {
            touched.push((c.id, false, true));
        }
        if out.len() >= limit {
            break;
        }
    }
    // Round robin over ready streams, no sleep to wait for future data.
    let ids: Vec<_> = s
        .streams
        .range((
            std::ops::Bound::Excluded(s.last_sent),
            std::ops::Bound::Unbounded,
        ))
        .chain(s.streams.range(..=s.last_sent))
        .map(|(&id, _)| id)
        .collect();
    loop {
        let mut progress = false;
        for &id in &ids {
            if out.len() + 8 >= limit {
                return Ok(touched);
            }
            let st = s.streams.get_mut(&id).unwrap();
            if st.terminal.is_some() || !st.opened {
                continue;
            }
            let mut share = if ready > 1 {
                inner.traffic.quantum_bytes.sample()
            } else {
                BLOCK
            };
            while share > 0 && st.send_credit > 0 {
                let Some(p) = st.output.front_mut() else {
                    break;
                };
                let n = p
                    .len()
                    .min(BLOCK)
                    .min(share)
                    .min(limit.saturating_sub(out.len() + 8));
                if n == 0 {
                    break;
                }
                frame(out, DATA, id, &p[..n]);
                p.advance(n);
                if p.is_empty() {
                    st.output.pop_front();
                }
                st.queued -= n;
                st.transferred += n;
                st.send_credit -= 1;
                st.inflight += 1;
                share -= n;
                touched.push((id, false, false));
                progress = true;
            }
            if st.local_fin && !st.fin_sent && st.output.is_empty() {
                frame(out, FIN, id, &[]);
                st.fin_sent = true;
                st.inflight += 1;
                touched.push((id, true, false));
                progress = true;
            }
            if let Some(w) = st.write_waker.take() {
                w.wake();
            }
            s.last_sent = id;
        }
        if !progress {
            break;
        }
    }
    Ok(touched)
}
async fn write_loop<W: AsyncWrite + Unpin>(
    mut w: W,
    inner: &Inner,
    idle: Duration,
) -> io::Result<()> {
    let mut out = Vec::with_capacity(BATCH + 1024);
    loop {
        let notified = inner.wake.notified();
        out.clear();
        let touched = batch(inner, &mut out)?;
        if out.is_empty() {
            notified.await;
            continue;
        }
        tokio::time::timeout(idle, async {
            w.write_all(&out).await?;
            w.flush().await
        })
        .await??;
        let mut s = inner.lock();
        for (id, fin, done) in touched {
            if let Some(st) = s.streams.get_mut(&id) {
                if done {
                    if st.terminal.is_none() {
                        st.terminal = Some(Ok(()));
                    }
                } else {
                    st.inflight = st.inflight.saturating_sub(1);
                }
                if fin {
                    st.fin_flushed = true;
                }
                st.activity = Instant::now();
                st.wake();
            }
        }
    }
}
async fn watchdog(inner: &Inner, idle: Duration, pool_idle: Duration) -> io::Result<()> {
    loop {
        tokio::time::sleep(Duration::from_secs(1)).await;
        let mut s = inner.lock();
        if let Some(e) = s.failure {
            return Err(e.into());
        }
        let mut expired = Vec::new();
        for (&id, st) in &mut s.streams {
            if st.terminal.is_none() && st.activity.elapsed() >= idle {
                st.fail(io::ErrorKind::TimedOut);
                expired.push(id);
            }
        }
        for id in expired {
            s.control(RESET, id, &[])?;
        }
        if s.active() == 0 && s.idle_since.elapsed() >= pool_idle {
            return Err(io::ErrorKind::TimedOut.into());
        }
        inner.wake.notify_one();
    }
}
