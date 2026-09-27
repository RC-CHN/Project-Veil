// Owned HTTPS workload and fresh Chrome driver. Uses only Node built-ins.
import fs from 'node:fs';
import http2 from 'node:http2';
import https from 'node:https';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {parseArgs} from 'node:util';

const {values: a} = parseArgs({options: Object.fromEntries(
  ['role', 'cert', 'key', 'port', 'protocol', 'browser', 'profile', 'log', 'url', 'proxy', 'spki']
    .map(name => [name, {type: 'string'}]))});

if (a.role === 'server') {
  const options = {key: fs.readFileSync(a.key), cert: fs.readFileSync(a.cert),
    minVersion: 'TLSv1.3', maxVersion: 'TLSv1.3', ecdhCurve: 'X25519'};
  const server = a.protocol === 'h2' ? http2.createSecureServer(options) : https.createServer(options);
  server.on('request', (req, res) => {
    const url = new URL(req.url, 'https://localhost');
    res.setHeader('Cache-Control', 'no-store');
    if (a.protocol === 'h1-close') res.setHeader('Connection', 'close');
    let body;
    if (url.pathname === '/run') {
      const parallel = url.searchParams.get('parallel') === '1';
      const seed = Number(url.searchParams.get('seed') || 0);
      const sizes = seed ? Array.from({length: 6}, (_, i) => 64 + ((seed * 1103515245 + i * 1234567) >>> 0) % 65536)
        : [64,4096,32768,64,4096,32768];
      body = `<!doctype html><meta charset="utf-8"><link rel="icon" href="data:,">
<title>Owned traffic fixture</title><script>
(async()=>{try {
  const sizes=${JSON.stringify(sizes)};
  const fetchOne=async(size,index)=>{
    const r=await fetch('/data/'+size+'?i='+index,{cache:'no-store'});
    const b=await r.text();if(!r.ok||b!=='x'.repeat(size))throw Error('body mismatch');
    return b.length;
  };
  const lengths=[];
  if(${parallel}) lengths.push(...await Promise.all(sizes.map(fetchOne)));
  else for(let i=0;i<sizes.length;i++) lengths.push(await fetchOne(sizes[i],i));
  globalThis.__veilResult={ok:true,lengths};
}catch(e){globalThis.__veilResult={ok:false,error:String(e)}}})();
</script>`;
      res.setHeader('Content-Type', 'text/html; charset=utf-8');
    } else if (/^\/data\/\d+$/.test(url.pathname)) {
      const size = Number(url.pathname.slice(6));
      if (size > 1 << 20) { res.writeHead(400); res.end(); return; }
      body = 'x'.repeat(size);
      res.setHeader('Content-Type', 'text/plain');
    } else { res.writeHead(404); res.end(); return; }
    res.setHeader('Content-Length', Buffer.byteLength(body));
    res.end(body);
    process.stdout.write(JSON.stringify({path: req.url, protocol: req.httpVersion, bytes: Buffer.byteLength(body)})+'\n');
  });
  server.on('tlsClientError', () => {}); // Readiness and REALITY cover connections.
  server.on('sessionError', () => {});
  server.listen(Number(a.port), '127.0.0.1');
} else if (a.role === 'browser') {
  const log = fs.openSync(a.log, 'w');
  const args = ['--headless', '--no-sandbox', '--disable-gpu', '--disable-quic',
    '--disable-background-networking', '--disable-component-update', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-pipe', `--user-data-dir=${a.profile}`,
    `--ignore-certificate-errors-spki-list=${a.spki}`];
  args.push('--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1');
  // Send only the dotless localhost fixture through SOCKS. Background FQDNs
  // bypass the proxy and fail DNS inside the isolated namespace.
  if (a.proxy) args.push(`--proxy-server=socks5://${a.proxy}`, '--proxy-bypass-list=*.*;<-loopback>');
  else args.push('--no-proxy-server');
  const child = spawn(a.browser, args, {stdio: ['ignore', log, log, 'pipe', 'pipe']});
  fs.closeSync(log);
  let sequence = 0, input = '';
  const pending = new Map(), responses = [], failures = [];
  child.stdio[4].setEncoding('utf8');
  child.stdio[4].on('data', part => {
    input += part;
    let end;
    while ((end = input.indexOf('\0')) >= 0) {
      const message = JSON.parse(input.slice(0, end)); input = input.slice(end + 1);
      if (message.id) {
        const p = pending.get(message.id);
        if (p) { pending.delete(message.id); clearTimeout(p.timer);
          if (message.error) p.reject(Error(JSON.stringify(message.error))); else p.resolve(message.result); }
      } else if (message.method === 'Network.responseReceived') {
        const r = message.params.response;
        if (r.url.startsWith('https://localhost:')) responses.push({url: r.url, protocol: r.protocol, connectionId: r.connectionId,
          connectionReused: r.connectionReused, status: r.status});
      } else if (message.method === 'Network.loadingFailed') {
        if (failures.length < 16) failures.push(message.params.errorText);
      }
    }
  });
  function send(method, params = {}, sessionId) {
    return new Promise((resolve, reject) => {
      const id = ++sequence;
      const timer = setTimeout(() => { pending.delete(id); reject(Error('CDP timeout: '+method)); }, 15000);
      pending.set(id, {resolve, reject, timer});
      child.stdio[3].write(JSON.stringify({id, method, params, sessionId})+'\0');
    });
  }
  try {
    const version = await send('Browser.getVersion');
    const {targetId} = await send('Target.createTarget', {url: 'about:blank'});
    const {sessionId} = await send('Target.attachToTarget', {targetId, flatten: true});
    await send('Page.enable', {}, sessionId);
    await send('Network.enable', {}, sessionId);
    await send('Network.setCacheDisabled', {cacheDisabled: true}, sessionId);
    await send('Page.navigate', {url: a.url}, sessionId);
    let result;
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      const r = await send('Runtime.evaluate', {expression: 'globalThis.__veilResult', returnByValue: true}, sessionId);
      result = r.result.value;
      if (result) break;
      await new Promise(resolve => setTimeout(resolve, 25));
    }
    if (!result?.ok || responses.some(r => r.status !== 200) ||
        responses.filter(r => r.url.startsWith(a.url.split('/run')[0])).length !== 7)
      throw Error('Browser workload failed: '+JSON.stringify({result, responses, failures}));
    const navigation = await send('Runtime.evaluate', {expression:
      '(()=>{const n=performance.getEntriesByType("navigation")[0];return {ttfb_ms:n.responseStart-n.startTime,connect_ms:n.connectEnd-n.connectStart}})()',
      returnByValue: true}, sessionId);
    process.stdout.write(JSON.stringify({version, result, responses, navigation: navigation.result.value})+'\n');
    await send('Browser.close').catch(() => {});
  } finally {
    for (const p of pending.values()) { clearTimeout(p.timer); p.reject(Error('browser closed')); }
    if (child.exitCode === null) {
      const exited = once(child, 'exit');
      child.kill('SIGTERM');
      const force = setTimeout(() => child.kill('SIGKILL'), 3000);
      await exited;
      clearTimeout(force);
    }
  }
} else {
  throw Error('role must be server or browser');
}
