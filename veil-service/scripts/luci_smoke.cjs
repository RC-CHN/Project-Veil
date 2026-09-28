// Run only against a disposable local OpenWrt test VM. No credentials are logged.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');

const env = process.env;
const url = new URL(env.VEIL_LUCI_URL || 'http://127.0.0.1:18084/cgi-bin/luci/admin/services/veil');
if (env.VEIL_LUCI_TEST !== '1' || !['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname)) {
 throw new Error('requires VEIL_LUCI_TEST=1 and a loopback test VM URL');
}
const zh = env.VEIL_LUCI_LANGUAGE === 'zh';
const labels = zh ? {
 start:'启动代理',stop:'停止代理',running:'运行中',stopped:'已停止',save:'保存配置',
 apply:'应用已保存配置',confirm:'应用并重启',cancel:'取消',imported:'配置已导入并通过校验，请确认后保存。',
 saved:'配置已保存。',invalid:'请导入代理客户端配置，不要设置固定转发目标。'
} : {
 start:'Start proxy',stop:'Stop proxy',running:'Running',stopped:'Stopped',save:'Save configuration',
 apply:'Apply saved configuration',confirm:'Apply and restart',cancel:'Cancel',
 imported:'Configuration imported and validated. Save it when ready.',saved:'Configuration saved.',
 invalid:'Import a proxy client configuration without a fixed forwarding target.'
};
(async () => {
 const output = path.resolve(env.VEIL_LUCI_OUTPUT || '.build/luci-review');
 await fs.mkdir(output,{recursive:true});
 const browser = await chromium.launch({executablePath:env.CHROMIUM_PATH,headless:true,args:['--no-sandbox']});
 const options = {viewport:{width:1360,height:1000},colorScheme:env.VEIL_LUCI_COLOR || 'light',locale:zh?'zh-CN':'en-US'};
 let context;
 try {
  const login = await browser.newContext(options);
  const auth = await login.newPage();
  await auth.goto(url.href);
  await auth.locator('input[name="luci_username"]').fill(env.VEIL_LUCI_USER || 'root');
  await auth.locator('input[name="luci_password"]').fill(env.VEIL_LUCI_PASSWORD);
  await auth.locator('input[name="luci_password"]').press('Enter');
  await auth.waitForSelector('.veil-page');
  const state = await login.storageState();
  await login.close();
  context = await browser.newContext({...options,storageState:state,recordVideo:env.VEIL_LUCI_VIDEO==='1'?{dir:output,size:options.viewport}:undefined});
  const page = await context.newPage();
  const errors=[]; page.on('pageerror',error=>errors.push(error.message));
  await page.goto(url.href);
  await page.waitForSelector('.veil-page');
  assert.equal(await page.locator('#veil-file').isVisible(),false,'native file input must stay hidden under every theme');
  if (env.VEIL_LUCI_READONLY === '1') {
   assert.equal(await page.locator('#veil-json').inputValue(), '');
   for (const field of ['#veil-json', '#veil-protocol', '#veil-address', '#veil-file']) assert.equal(await page.locator(field).isDisabled(), true);
   for (const name of ['start', 'stop', 'save', 'apply']) assert.equal(await page.getByRole('button', {name:labels[name],exact:true}).isDisabled(), true);
   const session = await page.evaluate(()=>L.env.sessionid);
   const denied = await context.request.post(new URL('/ubus',url).href,{data:{jsonrpc:'2.0',id:1,method:'call',params:[session,'veil','config',{}]}});
   const denial = await denied.json();
   assert.ok(denial.result?.[0] === 6 || denial.error?.code === -32002, 'rpcd must deny credential reads: ' + JSON.stringify(denial));
   await page.screenshot({path:path.join(output,'readonly.png'),fullPage:true});
   assert.deepEqual(errors, []);
   console.log('Read-only status view passed; configuration and mutation controls unavailable.');
   return;
  }
  const button = name=>page.getByRole('button',{name:labels[name],exact:true});
  const badge = state=>page.waitForFunction(value=>document.querySelector('.veil-badge').textContent===value,labels[state]);
  const pause = ()=>page.waitForTimeout(env.VEIL_LUCI_VIDEO==='1'?900:50);
  if(await button('stop').isEnabled()){await button('stop').click();await badge('stopped');}
  await pause();
  const cfg=JSON.parse(await fs.readFile(env.VEIL_LUCI_CONFIG,'utf8'));
  assert.equal(cfg.inbound,'mixed','test config must select mixed SOCKS5/HTTP');
  await page.locator('#veil-file').setInputFiles({name:'connection.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(cfg))});
  await page.getByText(labels.imported,{exact:true}).waitFor();
  await pause();
  await button('save').click();
  await page.getByText(labels.saved,{exact:true}).waitFor();
  await pause();
  await button('start').click();
  await badge('running');
  if(env.VEIL_HTTP_TARGET){
   for(const scheme of ['socks5h','http']){
    const proxy=scheme+'://'+env.VEIL_PROXY_ADDRESS;
    const reply=execFileSync('curl',['--fail','--silent','--show-error','--max-time','10','--noproxy','','--proxy',proxy,env.VEIL_HTTP_TARGET],{encoding:'utf8'});
    assert.match(reply,/Veil SOCKS5 \+ HTTP acceptance passed/);
    const secure=execFileSync('curl',['--fail','--silent','--show-error','--max-time','10','--noproxy','','--proxy',proxy,'--cacert',env.VEIL_TEST_CA,env.VEIL_HTTPS_TARGET],{encoding:'utf8'});
    assert.match(secure,/Veil SOCKS5 \+ HTTP acceptance passed/);
   }
  }
  await pause();
  await page.locator('#veil-protocol').selectOption('http');
  await pause();
  await button('save').click();
  await button('apply').click();
  await pause();
  await button('cancel').click();
  assert.equal(await page.locator('.veil-badge').innerText(),labels.running);
  await pause();
  await button('apply').click();
  await button('confirm').click();
  await page.waitForFunction(()=>!document.querySelector('.veil-notice').textContent);
  await pause();
  await button('stop').click();
  await badge('stopped');
  await pause();
  await page.screenshot({path:path.join(output,'desktop.png'),fullPage:true});
  await page.locator('#veil-file').setInputFiles({name:'invalid.json',mimeType:'application/json',buffer:Buffer.from('{"role":"server"}')});
  await page.getByText(labels.invalid,{exact:true}).waitFor();
  await pause();
  await page.screenshot({path:path.join(output,'error.png'),fullPage:true});
  assert.deepEqual(errors,[]);
  const video = page.video();
  await context.close();context=null;
  if(video) { await video.saveAs(path.join(output,'interaction.webm')); await video.delete(); }
  const mobile=await browser.newContext({...options,storageState:state,viewport:{width:390,height:844}});
  const screen=await mobile.newPage();
  await screen.goto(url.href);await screen.waitForSelector('.veil-page');
  assert.equal(await screen.evaluate(()=>document.querySelector('.veil-page').getBoundingClientRect().right>innerWidth),false);
  await screen.screenshot({path:path.join(output,'mobile-status.png'),fullPage:true});
  await screen.getByRole('button',{name:labels.save,exact:true}).scrollIntoViewIfNeeded();
  await screen.screenshot({path:path.join(output,'mobile-config.png'),fullPage:true});
  await mobile.close();
  console.log(JSON.stringify({result:'passed',language:zh?'zh':'en',color:options.colorScheme,traffic:!!env.VEIL_HTTP_TARGET,output}));
 } finally {if(context)await context.close();await browser.close();}
})().catch(error=>{console.error(error.message);process.exitCode=1;});
