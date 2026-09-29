// Real frontend assets/adapters, isolated control fixtures. Never changes a host proxy.
const assert = require('node:assert/strict'),
  fs = require('node:fs'),
  path = require('node:path'),
  http = require('node:http');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../..'),
  out = path.resolve(
    process.env.VEIL_UI_OUTPUT || path.join(root, '.build/ux-review'),
  );
const shared = path.join(root, 'veil-service/webui'),
  desktop = path.join(root, 'veil-desktop/frontend');
const opn = path.join(root, 'veil-service/platform/opnsense/src/opnsense'),
  luci = path.join(
    root,
    'veil-service/platform/openwrt/luci-app-veil/htdocs/luci-static/resources',
  );
const read = (f) => fs.readFileSync(f, 'utf8');
const config = {
  role: 'client',
  server: 'exit.example:443',
  secret: 'fixture-secret',
  tls: {
    mode: 'reality',
    server_name: 'cover.example',
    reality_public_key: 'fixture-public',
    short_id: '0123456789abcdef',
    fingerprints: ['chrome149', 'chrome133'],
    record_padding: true,
  },
  max_connections: 32,
};
function bridge({ platform, readonly = false, language = 'en', seed = [] }) {
  window.testCalls = [];
  window.testImport = '';
  window.testFault = '';
  window.testDelay = 0;
  const profiles = new Map(seed.map((p) => [p.id, structuredClone(p)])),
    revisions = new Map(seed.map((p) => [p.id, '1'])),
    running = new Map();
  let next = 2;
  const copy = (x) => structuredClone(x);
  const rows = () =>
    Array.from(profiles.values()).map((p) => {
      const actual = running.get(p.id),
        relay = profiles.get((actual || p).relay_id);
      const diagnostic = {
        started_at: '2026-09-29T03:00:00Z',
        connection_limit: 64,
        sequence: 1,
        recent: [
          {
            at: '2026-09-29T03:24:00Z',
            first_at: '2026-09-29T03:24:00Z',
            count: 1,
            stage: 'TLS handshake',
            code: 'timeout',
            message: 'TLS handshake timed out',
          },
        ],
        pool: { total: 1, idle: 1, streams: 0, limit: 64, dialing: false },
      };
      return {
        id: p.id,
        name: p.name,
        kind: p.kind,
        enabled: p.enabled,
        server: (actual || p).config.server,
        relay_id: (actual || p).relay_id,
        relay_name: relay?.name,
        relay_server: relay?.config.server,
        revision: revisions.get(p.id),
        state: p.kind === 'relay' ? 'ready' : actual ? 'running' : 'stopped',
        pending: !!actual && JSON.stringify(actual) !== JSON.stringify(p),
        inlets: copy((actual || p).inlets),
        stats: {
          accepted: 12,
          completed: 10,
          failed: 2,
          active_connections: 0,
          active_streams: 0,
        },
        diagnostics: diagnostic,
        ...(relay
          ? {
              relay: {
                state: 'running',
                stats: {},
                diagnostics: copy(diagnostic),
              },
            }
          : {}),
      };
    });
  const fail = (code, message) => ({ version: 1, error: { code, message } });
  window.testRequest = async (q) => {
    testCalls.push(copy(q));
    if (window.testDelay)
      await new Promise((r) => setTimeout(r, window.testDelay));
    if (readonly && q.action !== 'connections')
      return fail('denied', 'Permission denied');
    if (testFault && q.action !== 'connections') {
      const code = testFault;
      testFault = '';
      return fail(
        code,
        code === 'conflict'
          ? 'Configuration changed elsewhere'
          : 'Requested test failure',
      );
    }
    const id = q.profile?.id || q.id,
      old = profiles.get(id);
    if (q.action === 'connections') return { version: 1, connections: rows() };
    if (q.action === 'connection_get' || q.action === 'connection_export')
      return {
        version: 1,
        profile: copy(old),
        relay: old?.relay_id ? copy(profiles.get(old.relay_id)) : undefined,
      };
    if (q.action === 'connection_test')
      return {
        version: 1,
        probe: { ok: true, milliseconds: 125, at: new Date().toISOString() },
        connections: rows(),
      };
    if (q.expected_revision !== (revisions.get(id) || ''))
      return fail('conflict', 'Configuration changed elsewhere');
    if (q.action === 'connection_save') {
      const p = copy(q.profile);
      if (!p.name || !p.config.server || !p.config.secret)
        return fail('invalid_config', 'Missing required field');
      if (p.relay_id && !profiles.has(p.relay_id) && q.relay?.id !== p.relay_id)
        return fail('invalid_config', 'Relay missing');
      if (q.relay) {
        profiles.set(q.relay.id, copy(q.relay));
        revisions.set(q.relay.id, String(next++));
      }
      profiles.set(id, p);
      if (q.apply) {
        if (p.enabled && p.kind === 'connection') running.set(id, copy(p));
        else running.delete(id);
      }
    } else if (q.action === 'connection_start') {
      old.enabled = true;
      running.set(id, copy(old));
    } else if (q.action === 'connection_stop') {
      old.enabled = false;
      running.delete(id);
    } else if (q.action === 'connection_delete') {
      if (Array.from(profiles.values()).some((p) => p.relay_id === id))
        return fail('in_use', 'Relay is in use');
      profiles.delete(id);
      running.delete(id);
    }
    revisions.set(id, String(next++));
    return { version: 1, revision: revisions.get(id), connections: rows() };
  };
  if (platform === 'desktop') {
    let proxy = {
      mode: 'keep',
      connection: '',
      supported: true,
      managed: false,
    };
    window.go = {
      main: {
        App: {
          Request: testRequest,
          SystemProxy: async () => copy(proxy),
          SetLanguage: async () => {},
          PrepareConnections: async () => {},
          Import: async () => testImport,
          Export: async (id) => {
            window.testExport = await testRequest({
              version: 1,
              action: 'connection_export',
              id,
            });
          },
          SetSystemProxyConnection: async (id) => {
            proxy.connection = id;
          },
          SetSystemProxyMode: async (mode) => {
            proxy.mode = mode;
            proxy.managed = mode === 'auto';
          },
          ClearSystemProxy: async () => {
            proxy.mode = 'keep';
            proxy.managed = false;
          },
          Quit: async () => {
            window.testQuit = true;
          },
        },
      },
    };
    window.runtime = { EventsOn: () => {}, WindowMaximise: () => {} };
    localStorage.setItem('language', language);
  } else if (platform === 'opn') {
    window.veilUI = { writable: !readonly, language };
    window.$ = (fn) => document.addEventListener('DOMContentLoaded', fn);
    window.ajaxGet = async (url) =>
      url.endsWith('/connections')
        ? testRequest({ version: 1, action: 'connections' })
        : url.endsWith('/settings/get')
          ? { profile: '', revision: '', enabled: false }
          : {
              configured: false,
              pending: false,
              status: { state: 'stopped', stats: {} },
            };
    window.ajaxCall = async (url, data) =>
      url.endsWith('/connection')
        ? testRequest(JSON.parse(data.request))
        : { revision: '1' };
  } else {
    window.E = (tag, attrs = {}, children = []) => {
      const n = document.createElement(tag);
      for (const [k, v] of Object.entries(attrs)) {
        if (typeof v === 'function') n.addEventListener(k, v);
        else if (v !== false && v != null)
          n.setAttribute(k, v === true ? '' : v);
      }
      for (const c of [children].flat(Infinity))
        if (c != null)
          n.append(c instanceof Node ? c : document.createTextNode(String(c)));
      return n;
    };
    window.L = {
      hasViewPermission: () => !readonly,
      resource: (p) => '/luci/' + p,
    };
    window.rpc = {
      declare:
        (spec) =>
        (...args) =>
          testRequest({
            version: 1,
            action: spec.method,
            ...Object.fromEntries(spec.params.map((p, i) => [p, args[i]])),
          }),
    };
    window.view = {
      extend: (v) => {
        window.testView = v;
        return v;
      },
    };
  }
}
const baseCSS =
  'body{font:14px/1.5 system-ui,sans-serif;background:#f5f6f8;color:#303944;margin:24px}*{box-sizing:border-box}[hidden]{display:none!important}button,summary{cursor:pointer}button,input,textarea,select{font:inherit}.content-box{background:white}.form-control{width:100%}.btn{padding:8px 14px;border:1px solid #bfc6ca;border-radius:6px;background:white;color:inherit}.btn-primary{background:#cc6533;color:white}.text-muted{color:#61727c}body[data-darkmode=true]{background:#18202b;color:#dce1e9;--surface:#253040;--cbi-section-bg:#253040;--line:#445164;--accent:#7eb6e8}';
function platformTheme(platform, dark = false) {
  return platform === 'luci'
    ? path.join(
        process.env.VEIL_ARGON_ASSETS || '',
        dark ? 'dark.css' : 'cascade.css',
      )
    : process.env.VEIL_OPN_CSS;
}
const server = http.createServer((req, res) => {
  try {
    const u = new URL(req.url, 'http://local');
    let content,
      type = 'text/html';
    const lang = u.searchParams.get('lang') === 'zh' ? 'zh-CN' : 'en';
    if (u.pathname === '/theme.css') {
      content = read(
        platformTheme(
          u.searchParams.get('platform'),
          u.searchParams.get('dark') === '1',
        ),
      );
      type = 'text/css';
    } else if (u.pathname === '/desktop/')
      content = read(path.join(desktop, 'index.html'));
    else if (u.pathname.startsWith('/desktop/')) {
      const name = path.basename(u.pathname);
      content = read(path.join(desktop, name));
      type = name.endsWith('.css') ? 'text/css' : 'application/javascript';
    } else if (
      ['/shared/', '/luci/veil/', '/ui/js/veil/shared/'].some((p) =>
        u.pathname.startsWith(p),
      )
    ) {
      const name = path.basename(u.pathname);
      if (!['connections.js', 'connections.css', 'i18n.js'].includes(name))
        throw Error('unknown');
      content = read(path.join(shared, name));
      type = name.endsWith('.css') ? 'text/css' : 'application/javascript';
    } else if (u.pathname === '/opn/') {
      content =
        '<html lang="' +
        lang +
        '"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/base.css"><link rel="stylesheet" href="/opn/veil.css"><body>' +
        read(path.join(opn, 'mvc/app/views/OPNsense/Veil/index.volt')).replace(
          /<script>window.veilUI[\s\S]*?<\/script>/,
          '',
        ) +
        '</body></html>';
    } else if (u.pathname === '/ui/js/veil/page.js') {
      content = read(path.join(opn, 'www/js/veil/page.js'));
      type = 'application/javascript';
    } else if (u.pathname === '/opn/veil.css') {
      content = read(path.join(opn, 'www/css/veil.css'));
      type = 'text/css';
    } else if (u.pathname === '/luci/')
      content =
        '<html lang="' +
        lang +
        '"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/base.css"><body><main id="view"></main><script src="/luci/view.js"></script></body></html>';
    else if (u.pathname === '/luci/view.js') {
      content =
        '(function(){' +
        read(path.join(luci, 'view/veil.js')) +
        '})();testView.load().then(()=>document.getElementById("view").append(testView.render()));';
      type = 'application/javascript';
    } else if (u.pathname === '/base.css') {
      content = baseCSS;
      type = 'text/css';
    } else throw Error('not found');
    res.writeHead(200, { 'Content-Type': type });
    res.end(content);
  } catch {
    res.writeHead(404);
    res.end();
  }
});
(async () => {
  fs.mkdirSync(out, { recursive: true });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const base = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH,
    headless: true,
    args: ['--no-sandbox'],
  });
  try {
    for (const platform of ['desktop', 'luci', 'opn']) {
      const context = await browser.newContext({
        viewport: { width: 1180, height: 1000 },
        recordVideo:
          platform === 'desktop'
            ? { dir: out, size: { width: 1180, height: 1000 } }
            : undefined,
      });
      await context.addInitScript(bridge, { platform });
      const page = await context.newPage(),
        errors = [];
      page.on('pageerror', (e) => errors.push(e.message));
      page.setDefaultTimeout(6000);
      const button = (name, where = page) =>
        where.getByRole('button', { name, exact: true });
      const dialog = page.locator('dialog.veil-dialog');
      const card = (name) =>
        page
          .locator('.veil-card')
          .filter({ has: page.getByRole('heading', { name, exact: true }) });
      const save = async (apply = false) => {
        await button(apply ? 'Save and apply' : 'Save only', dialog).click();
        await dialog.waitFor({ state: 'hidden' });
      };
      await page.goto(base + '/' + platform + '/');
      await page.locator('.veil-page').waitFor();
      if (
        (platform === 'luci' && process.env.VEIL_ARGON_ASSETS) ||
        (platform === 'opn' && process.env.VEIL_OPN_CSS)
      )
        await page.addStyleTag({
          url: base + '/theme.css?platform=' + platform,
        });
      await button('Add connection').click();
      await dialog.getByLabel('Name', { exact: true }).fill('First');
      assert(
        await page.evaluate(() => {
          const ids = Array.from(
            document.querySelectorAll('[id]'),
            (n) => n.id,
          );
          return new Set(ids).size === ids.length;
        }),
        platform + ' duplicate element IDs',
      );
      await dialog
        .getByLabel('Server address', { exact: true })
        .fill('exit.example');
      await dialog
        .getByLabel('Authentication secret', { exact: true })
        .fill('fixture-secret');
      await dialog
        .getByLabel('Server name (SNI)', { exact: true })
        .fill('cover.example');
      await dialog
        .getByLabel('REALITY public key', { exact: true })
        .fill('fixture-public');
      await dialog
        .getByLabel('Short ID', { exact: true })
        .fill('0123456789abcdef');
      await page.screenshot({ path: path.join(out, platform + '-editor.png') });
      await save(false);
      await card('First').getByText('Stopped', { exact: true }).waitFor();
      await button('Start', card('First')).click();
      await card('First').getByText('Proxy started', { exact: true }).waitFor();
      await button('Edit', card('First')).click();
      await dialog
        .getByLabel('Server address', { exact: true })
        .fill('edited.example');
      await save(false);
      await card('First')
        .getByText(/Saved changes are waiting/)
        .waitFor();
      assert.equal(
        await card('First').locator('.veil-server').textContent(),
        'exit.example:443',
      );
      await button('Apply changes', card('First')).click();
      await button('Apply changes', dialog).click();
      await card('First')
        .getByText('edited.example:443', { exact: true })
        .waitFor();
      await button('Edit', card('First')).click();
      await dialog.locator('details > summary').click();
      const advanced = dialog.locator('details textarea');
      await advanced.fill(JSON.stringify(config));
      assert.equal(
        await dialog.getByLabel('Server address', { exact: true }).inputValue(),
        'exit.example',
      );
      await dialog
        .getByLabel('Server address', { exact: true })
        .fill('third.example');
      assert.equal(JSON.parse(await advanced.inputValue()).max_connections, 32);
      await advanced.fill('{');
      assert.equal(
        await dialog.getByLabel('Server address', { exact: true }).isDisabled(),
        true,
      );
      await button('Save only', dialog).click();
      assert.equal(await advanced.inputValue(), '{');
      await advanced.fill(JSON.stringify(config));
      await page.keyboard.press('Escape');
      await dialog
        .getByText('Discard unsaved changes?', { exact: true })
        .waitFor();
      await button('Keep editing', dialog).click();
      await page.evaluate(() => {
        testFault = 'conflict';
      });
      await button('Save only', dialog).click();
      await dialog
        .getByText('Configuration changed elsewhere', { exact: true })
        .waitFor();
      assert.equal(
        JSON.parse(await advanced.inputValue()).server,
        'exit.example:443',
      );
      await page.evaluate(() => {
        testDelay = 400;
      });
      await button('Save only', dialog).click();
      assert.equal(
        await dialog.getByLabel('Name', { exact: true }).isDisabled(),
        true,
      );
      await dialog.waitFor({ state: 'hidden' });
      await page.evaluate(() => {
        testDelay = 0;
      });
      const relay = {
        id: 'relay-origin',
        name: 'Shared relay',
        kind: 'relay',
        enabled: false,
        config: { ...config, server: 'relay.example:443' },
        inlets: [],
      };
      const bundle = {
        version: 1,
        relay,
        profile: {
          id: 'imported',
          name: 'Via relay',
          kind: 'connection',
          enabled: false,
          relay_id: relay.id,
          config,
          inlets: [
            { protocol: 'mixed', listen: '127.0.0.1:1090' },
            { protocol: 'http', listen: '127.0.0.1:8090' },
          ],
        },
      };
      await button('Paste JSON').click();
      await dialog
        .getByRole('textbox', { name: 'Connection JSON' })
        .fill(JSON.stringify(bundle));
      await button('Import', dialog).click();
      await dialog.getByLabel('Name', { exact: true }).waitFor();
      await dialog.locator('#veil-enabled').check();
      await save(true);
      await card('Via relay')
        .getByText('relay.example:443', { exact: true })
        .waitFor();
      assert.equal(
        await card('Via relay').locator('.veil-endpoint').count(),
        2,
      );
      await button('Test Google', card('Via relay')).click();
      assert(
        await page.evaluate(() =>
          testCalls.some((q) => q.action === 'connection_test'),
        ),
      );
      await card('Via relay').locator('summary').click();
      await button('Diagnostics', card('Via relay')).click();
      await dialog
        .getByRole('heading', { name: 'Relay tunnel', exact: true })
        .waitFor();
      await page.screenshot({
        path: path.join(out, platform + '-diagnostics.png'),
      });
      await button('Close', dialog).click();
      await button('Edit', card('Via relay')).click();
      await dialog.getByLabel('Name', { exact: true }).fill('Discard me');
      await button('Cancel', dialog).click();
      await button('Discard', dialog).click();
      assert.equal(await card('Via relay').count(), 1);
      await button('Stop', card('Via relay')).click();
      await button('Stop', dialog).click();
      await card('Via relay').getByText('Stopped', { exact: true }).waitFor();
      await card('Via relay').locator('summary').click();
      await button('Export', card('Via relay')).click();
      if (platform === 'desktop') {
        await page.waitForFunction(() => !!window.testExport);
        assert.equal(
          await page.evaluate(() => testExport.relay.config.server),
          'relay.example:443',
        );
      }
      await page.screenshot({
        path: path.join(out, platform + '-connections.png'),
        fullPage: true,
      });
      await page.setViewportSize({ width: 420, height: 900 });
      assert(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
        platform + ' narrow overflow',
      );
      await page.screenshot({
        path: path.join(out, platform + '-narrow.png'),
        fullPage: true,
      });
      assert.deepEqual(errors, [], platform + ' script errors');
      await context.close();
      console.log(platform + ' complete connection journeys passed');
    }
    for (const platform of ['desktop', 'luci', 'opn']) {
      const seed = [
        {
          id: 'test',
          name: '洛杉矶',
          kind: 'connection',
          enabled: false,
          config,
          inlets: [{ protocol: 'mixed', listen: '127.0.0.1:1080' }],
        },
      ];
      const ctx = await browser.newContext({
        viewport: { width: 1180, height: 1000 },
        colorScheme: 'dark',
      });
      await ctx.addInitScript(bridge, {
        platform,
        language: 'zh',
        seed,
        readonly: platform !== 'desktop',
      });
      const page = await ctx.newPage();
      await page.goto(base + '/' + platform + '/?lang=zh');
      await page.locator('.veil-card').waitFor();
      if (platform === 'luci' && process.env.VEIL_ARGON_ASSETS) {
        await page.addStyleTag({ url: base + '/theme.css?platform=luci' });
        await page.addStyleTag({
          url: base + '/theme.css?platform=luci&dark=1',
        });
      }
      if (platform !== 'desktop') {
        await page.evaluate(() => (document.body.dataset.darkmode = 'true'));
        assert(
          await page
            .getByRole('button', { name: '添加连接', exact: true })
            .isDisabled(),
        );
      }
      await page.locator('.veil-card summary').click();
      await page.getByRole('button', { name: '诊断', exact: true }).click();
      await page
        .getByRole('heading', { name: '出口隧道', exact: true })
        .waitFor();
      await page.screenshot({
        path: path.join(out, platform + '-zh-dark.png'),
      });
      await ctx.close();
    }
  } finally {
    await browser.close();
    server.close();
  }
  console.log(
    'All shared UI journeys and CN/EN readonly diagnostics passed (mock control).',
  );
})().catch((error) => {
  console.error(error);
  server.close();
  process.exitCode = 1;
});
