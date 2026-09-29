// Acceptance against an explicitly selected disposable OPNsense VM.
'use strict';
const assert = require('node:assert/strict'),
  fs = require('node:fs/promises'),
  path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const env = process.env,
  url = new URL(env.VEIL_OPNSENSE_URL || 'https://127.0.0.1:18443/ui/veil/');
if (
  env.VEIL_OPNSENSE_TEST !== '1' ||
  !['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname)
)
  throw Error('requires VEIL_OPNSENSE_TEST=1 and a loopback test VM URL');
const zh = env.VEIL_OPNSENSE_LANGUAGE === 'zh';
const labels = zh
  ? {
      paste: '粘贴 JSON',
      import: '导入',
      save: '保存并应用',
      edit: '编辑',
      stop: '停止',
      stopped: '已停止',
      running: '代理已启动',
      cancel: '取消',
      apply: '应用更改',
    }
  : {
      paste: 'Paste JSON',
      import: 'Import',
      save: 'Save and apply',
      edit: 'Edit',
      stop: 'Stop',
      stopped: 'Stopped',
      running: 'Proxy started',
      cancel: 'Cancel',
      apply: 'Apply changes',
    };
(async () => {
  const output = path.resolve(
    env.VEIL_OPNSENSE_OUTPUT || '.build/opnsense-review',
  );
  await fs.mkdir(output, { recursive: true });
  const browser = await chromium.launch({
    executablePath: env.CHROMIUM_PATH,
    headless: true,
    args: ['--no-sandbox'],
  });
  let context;
  try {
    const options = {
      ignoreHTTPSErrors: true,
      viewport: { width: 1400, height: 1000 },
    };
    const login = await browser.newContext(options),
      auth = await login.newPage();
    await auth.goto(url.href);
    await auth
      .locator('[name=usernamefld]')
      .fill(env.VEIL_OPNSENSE_USER || 'root');
    await auth.locator('[name=passwordfld]').fill(env.VEIL_OPNSENSE_PASSWORD);
    await auth.locator('[name=passwordfld]').press('Enter');
    await auth.locator('.veil-page').waitFor();
    const state = await login.storageState();
    await login.close();
    context = await browser.newContext({
      ...options,
      storageState: state,
      recordVideo:
        env.VEIL_OPNSENSE_VIDEO === '1'
          ? { dir: output, size: options.viewport }
          : undefined,
    });
    const page = await context.newPage(),
      errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(url.href);
    await page.locator('.veil-page').waitFor();
    if (env.VEIL_OPNSENSE_READONLY === '1') {
      assert(
        await page
          .getByRole('button', {
            name: zh ? '添加连接' : 'Add connection',
            exact: true,
          })
          .isDisabled(),
      );
      const allowed = await context.request.get(
        new URL('/api/veil/service/connections', url).href,
        { maxRedirects: 0 },
      );
      assert.equal(allowed.status(), 200);
      assert.equal((await allowed.json()).version, 1);
      const denied = await context.request.get(
        new URL('/api/veil/settings/get', url).href,
        { maxRedirects: 0 },
      );
      assert([302, 403].includes(denied.status()));
      await page.screenshot({
        path: path.join(output, 'readonly.png'),
        fullPage: true,
      });
      return;
    }
    const config = JSON.parse(
      await fs.readFile(env.VEIL_OPNSENSE_CONFIG, 'utf8'),
    );
    assert.equal(config.inbound, 'mixed');
    const dialog = page.locator('dialog.veil-dialog');
    const button = (name, scope = page) =>
      scope.getByRole('button', { name, exact: true });
    await button(labels.paste).click();
    await dialog.locator('textarea').fill(JSON.stringify(config));
    await button(labels.import, dialog).click();
    await dialog
      .getByLabel(zh ? '名称' : 'Name', { exact: true })
      .fill('Acceptance');
    await dialog.locator('#veil-enabled').check();
    await button(labels.save, dialog).click();
    await dialog.waitFor({ state: 'hidden' });
    const card = page
      .locator('.veil-card')
      .filter({
        has: page.getByRole('heading', { name: 'Acceptance', exact: true }),
      });
    await card.getByText(labels.running, { exact: true }).waitFor();
    await button(labels.edit, card).click();
    await dialog
      .getByLabel(zh ? '名称' : 'Name', { exact: true })
      .fill('Acceptance saved');
    await button(zh ? '仅保存' : 'Save only', dialog).click();
    await dialog.waitFor({ state: 'hidden' });
    const selected = page
      .locator('.veil-card')
      .filter({
        has: page.getByRole('heading', {
          name: 'Acceptance saved',
          exact: true,
        }),
      });
    await button(labels.stop, selected).click();
    await button(labels.cancel, dialog).click();
    await selected.getByText(labels.running, { exact: true }).waitFor();
    await page.screenshot({
      path: path.join(output, 'running.png'),
      fullPage: true,
    });
    const conflicts = await page.evaluate(async () => {
      const list = await ajaxGet('/api/veil/service/connections', {}),
        row = list.connections.find((r) => r.name === 'Acceptance saved');
      const send = (q) =>
        ajaxCall('/api/veil/settings/connection', {
          request: JSON.stringify({ version: 1, ...q }),
        });
      const current = await send({ action: 'connection_get', id: row.id });
      current.profile.name = 'Updated elsewhere';
      const saved = await send({
        action: 'connection_save',
        profile: current.profile,
        expected_revision: row.revision,
      });
      const stale = await send({
        action: 'connection_stop',
        id: row.id,
        expected_revision: row.revision,
      });
      return {
        saved: !saved.error,
        code: stale.error?.code,
        id: row.id,
        revision: saved.revision,
      };
    });
    assert.equal(conflicts.saved, true);
    assert.equal(conflicts.code, 'conflict');
    await page.evaluate(
      async ({ id, revision }) =>
        ajaxCall('/api/veil/settings/connection', {
          request: JSON.stringify({
            version: 1,
            action: 'connection_stop',
            id,
            expected_revision: revision,
          }),
        }),
      conflicts,
    );
    await page.reload();
    await page
      .locator('.veil-card')
      .getByText(labels.stopped, { exact: true })
      .first()
      .waitFor();
    await page.setViewportSize({ width: 420, height: 900 });
    assert(
      await page.evaluate(
        () =>
          document.querySelector('.veil-page').getBoundingClientRect().right <=
          innerWidth + 1,
      ),
    );
    await page.screenshot({
      path: path.join(output, 'mobile.png'),
      fullPage: true,
    });
    assert.deepEqual(errors, []);
    console.log(
      'OPNsense native connection lifecycle, revision guards and layout passed',
    );
  } finally {
    if (context) await context.close();
    await browser.close();
  }
})().catch((e) => {
  console.error(e.message);
  process.exitCode = 1;
});
