import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const fixture = JSON.parse(fs.readFileSync(process.argv[2]));
const helper = fileURLToPath(new URL('./windows.py', import.meta.url));
const native = (action) => {
  const out = execFileSync(fixture.python, [helper, action], {encoding: 'utf8', timeout: 10000});
  return out.trim() ? JSON.parse(out) : null;
};
const browser = await chromium.connectOverCDP(`http://127.0.0.1:${fixture.cdp}`);
const context = browser.contexts()[0];
const page = context.pages()[0] || await context.waitForEvent('page');
page.setDefaultTimeout(10000);
async function idle() {
  await page.waitForFunction(() => !document.querySelector('#quit').disabled);
  assert.equal(await page.locator('#feedback.error:visible').count(), 0, await page.locator('#feedback').textContent());
}
async function click(id) {await page.locator('#' + id).click(); await idle();}
async function confirm() {
  await page.locator('#confirmation').waitFor({state: 'visible'});
  assert.equal(await page.evaluate(() => document.activeElement.id), 'confirmation-cancel');
  await page.locator('#confirmation-accept').click(); await idle();
}
async function mode(value) {
  await page.locator('#proxy-mode').selectOption(value);
  if (value === 'auto') await confirm(); else await idle();
}
async function screenshot(name) {
  await page.screenshot({path: path.join(fixture.artifacts, name + '.png')});
}
function transfers() {
  for (const proxy of [`http://127.0.0.1:${fixture.proxy}`, `socks5h://127.0.0.1:${fixture.proxy}`]) {
    for (const scheme of ['http', 'https']) {
      const body = execFileSync('curl.exe', ['--silent', '--show-error', '--fail', '--max-time', '10',
        '--noproxy', '', '--proxy', proxy, '--cacert', fixture.cert,
        `${scheme}://127.0.0.1:${fixture[scheme]}/accept`], {encoding: 'utf8', timeout: 15000});
      assert.equal(body, 'Veil Windows GUI acceptance\n');
    }
  }
}
try {
  await page.waitForSelector('#language'); await idle();
  await page.locator('#language').selectOption('zh');
  await page.locator('#advanced > summary').click();
  await page.locator('#config').fill(fs.readFileSync(fixture.config, 'utf8'));
  await click('validate'); await click('save');
  await page.locator('#advanced > summary').click();
  await click('toggle');
  await page.waitForFunction(() => document.querySelector('#state').classList.contains('running'));
  assert.deepEqual(native('proxy'), fixture.baseline); transfers();
  await mode('auto'); assert.equal(native('proxy').flags, 3);
  assert(native('proxy').server.includes(`127.0.0.1:${fixture.proxy}`)); transfers();
  await page.evaluate(() => window.scrollTo(0, 0)); await screenshot('connected-zh');
  native('close');
  await page.waitForTimeout(300);
  assert.equal(native('window').visible, false, 'native window close must hide, not exit');
  transfers(); assert.equal(native('proxy').flags, 3);
  native('show');
  assert.equal(native('window').visible, true);
  await mode('keep'); assert.deepEqual(native('proxy'), fixture.baseline); transfers();
  await mode('auto'); await click('toggle'); assert.deepEqual(native('proxy'), fixture.baseline);
  await click('toggle'); assert.equal(native('proxy').flags, 3);
  await page.locator('#quit').click(); await page.locator('#confirmation').waitFor({state:'visible'});
  await screenshot('quit-zh'); await page.keyboard.press('Escape'); await idle(); transfers();
  await page.locator('#language').selectOption('en'); await click('theme');
  native('resize'); await page.waitForTimeout(300);
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'narrow layout overflows');
  await page.locator('#quit').click(); await screenshot('quit-en-dark-narrow');
  await page.locator('#confirmation-cancel').click(); await idle();
  await page.locator('#proxy-clear').click(); await confirm();
  assert.equal(native('proxy').flags, 1); assert.equal(native('proxy').server, '');
  assert.equal(await page.locator('#proxy-mode').inputValue(), 'keep'); transfers();
  fs.writeFileSync(path.join(fixture.artifacts, 'cleared-proxy.json'), JSON.stringify(native('proxy')));
  // After clear, automatic mode must restore that direct baseline on exit.
  await mode('auto');
  await page.locator('#quit').click();
  await page.locator('#confirmation-accept').click();
} catch (error) {
  await screenshot('failure').catch(() => {});
  throw error;
} finally {
  await browser.close();
}
