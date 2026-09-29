// Destructive acceptance suite: disposable local OpenWrt VM only.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const env = process.env;
const url = new URL(env.VEIL_LUCI_URL || 'http://127.0.0.1:18084/cgi-bin/luci/admin/services/veil');
if (env.VEIL_LUCI_TEST !== '1' || !['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname))
	throw new Error('requires VEIL_LUCI_TEST=1 and a loopback test VM URL');
const zh = env.VEIL_LUCI_LANGUAGE === 'zh';
const labels = zh
	? {
			save: '保存并应用',
			edit: '编辑',
			apply: '应用更改',
			stop: '停止',
			running: '代理已启动',
			disabled: '已停止',
		}
	: {
			save: 'Save and apply',
			edit: 'Edit',
			apply: 'Apply changes',
			stop: 'Stop',
			running: 'Proxy started',
			disabled: 'Stopped',
		};
(async () => {
	const output = path.resolve(env.VEIL_LUCI_OUTPUT || '.build/luci-review');
	await fs.mkdir(output, { recursive: true });
	const browser = await chromium.launch({
		executablePath: env.CHROMIUM_PATH,
		headless: true,
		args: ['--no-sandbox'],
	});
	try {
		const options = {
			viewport: { width: 1360, height: 1000 },
			colorScheme: env.VEIL_LUCI_COLOR || 'light',
			locale: zh ? 'zh-CN' : 'en-US',
		};
		const auth = await browser.newContext(options);
		const login = await auth.newPage();
		await login.goto(url.href);
		await login.locator('[name=luci_username]').fill(env.VEIL_LUCI_USER || 'root');
		await login.locator('[name=luci_password]').fill(env.VEIL_LUCI_PASSWORD);
		await login.locator('[name=luci_password]').press('Enter');
		await login.waitForSelector('.veil-page');
		const state = await auth.storageState();
		await auth.close();
		const context = await browser.newContext({
			...options,
			storageState: state,
			recordVideo:
				env.VEIL_LUCI_VIDEO === '1' ? { dir: output, size: options.viewport } : undefined,
		});
		const page = await context.newPage();
		const errors = [];
		page.on('pageerror', (e) => errors.push(e.message));
		await page.goto(url.href);
		await page.waitForSelector('.veil-page');
		const rpc = async (method, args = {}) =>
			page.evaluate(
				async ({ method, args }) => {
					const r = await fetch('/ubus', {
						method: 'POST',
						headers: { 'Content-Type': 'application/json' },
						body: JSON.stringify({
							jsonrpc: '2.0',
							id: 1,
							method: 'call',
							params: [L.env.sessionid, 'veil', method, args],
						}),
					});
					return r.json();
				},
				{ method, args },
			);
		if (env.VEIL_LUCI_READONLY === '1') {
			for (const name of zh
				? ['添加连接', '添加中转', '导入']
				: ['Add connection', 'Add relay', 'Import'])
				assert.equal(await page.getByRole('button', { name, exact: true }).isDisabled(), true);
			for (const method of ['connection_get', 'connection_test', 'connection_save']) {
				const r = await rpc(method, { id: 'default' });
				assert.ok(r.result?.[0] === 6 || r.error?.code === -32002, 'read-only RPC not denied');
			}
			await page.screenshot({ path: path.join(output, 'readonly.png'), fullPage: true });
			await context.close();
			return;
		}
		// Stop only the legacy single-instance proxy in this disposable VM so its port can migrate.
		await rpc('stop');
		const before = await rpc('connections');
		const baseline = new Set((before.result?.[1]?.connections || []).map((x) => x.id));
		await page.locator('#veil-import').setInputFiles(env.VEIL_LUCI_CONFIG);
		await page.locator('#veil-name').fill('Veil UI acceptance');
		await page.locator('#veil-enabled').check();
		await page.getByRole('button', { name: labels.save, exact: true }).click();
		await page.locator('.veil-card h3').filter({ hasText: 'Veil UI acceptance' }).waitFor();
		const card = page
			.locator('.veil-card')
			.filter({ has: page.getByRole('heading', { name: 'Veil UI acceptance', exact: true }) });
		await card.getByText(labels.running, { exact: true }).waitFor();
		if (env.VEIL_HTTP_TARGET) {
			for (const scheme of ['socks5h', 'http']) {
				const reply = execFileSync(
					'curl',
					[
						'--noproxy',
						'',
						'-fsS',
						'--max-time',
						'10',
						'--proxy',
						scheme + '://' + env.VEIL_PROXY_ADDRESS,
						env.VEIL_HTTP_TARGET,
					],
					{ encoding: 'utf8' },
				);
				assert.match(reply, /Veil SOCKS5 \+ HTTP acceptance passed/);
			}
		}
		await card.getByRole('button', { name: labels.edit, exact: true }).click();
		await page.locator('#veil-name').fill('Veil renamed');
		await page.getByRole('button', { name: labels.save, exact: true }).click();
		if (await page.getByRole('button', { name: labels.apply, exact: true }).isVisible())
			await page.getByRole('button', { name: labels.apply, exact: true }).click();
		await page.getByRole('heading', { name: 'Veil renamed', exact: true }).waitFor();
		const renamed = page
			.locator('.veil-card')
			.filter({ has: page.getByRole('heading', { name: 'Veil renamed', exact: true }) });
		await renamed.getByRole('button', { name: labels.stop, exact: true }).click();
		await page.locator('dialog.veil-dialog').getByRole('button', { name: labels.stop, exact: true }).click();
		await renamed.getByText(labels.disabled, { exact: true }).waitFor();
		await page.screenshot({ path: path.join(output, 'desktop.png'), fullPage: true });
		await page.setViewportSize({ width: 390, height: 844 });
		await page.reload();
		await page.waitForSelector('.veil-page');
		assert.equal(
			await page
				.locator('.veil-card')
				.evaluateAll((es) => es.some((e) => e.getBoundingClientRect().right > innerWidth)),
			false,
		);
		await page.screenshot({ path: path.join(output, 'mobile.png') });
		const after = await rpc('connections');
		for (const row of after.result?.[1]?.connections || []) {
			if (!baseline.has(row.id)) {
				const r = await rpc('connection_delete', { id: row.id, expected_revision: row.revision });
				assert.equal(r.result?.[1]?.error, undefined);
			}
		}
		assert.deepEqual(errors, []);
		await context.close();
		console.log(
			'Multi-connection LuCI import, save/apply, rename, proxy and mobile checks passed.',
		);
	} finally {
		await browser.close();
	}
})().catch((e) => {
	console.error(e);
	process.exitCode = 1;
});
