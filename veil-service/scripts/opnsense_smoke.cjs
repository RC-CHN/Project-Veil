// Real-browser acceptance against an explicitly selected disposable test VM.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const env = process.env;
const url = new URL(env.VEIL_OPNSENSE_URL || 'https://127.0.0.1:18443/ui/veil/');
if (env.VEIL_OPNSENSE_TEST !== '1' || !['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname)) {
    throw new Error('requires VEIL_OPNSENSE_TEST=1 and a loopback test VM URL');
}
const zh = env.VEIL_OPNSENSE_LANGUAGE === 'zh';
const labels = zh ? {
    running:'运行中',stopped:'已停止',saved:'配置已保存。',imported:'配置已导入并通过校验，请确认后保存。',
    cancel:'取消',apply:'应用并重启',invalid:'JSON 格式有误，请检查后再保存。'
} : {
    running:'Running',stopped:'Stopped',saved:'Configuration saved.',imported:'Configuration imported and validated. Save it when ready.',
    cancel:'Cancel',apply:'Apply and restart',invalid:'Invalid JSON. Check the configuration before saving.'
};
(async () => {
    const output = path.resolve(env.VEIL_OPNSENSE_OUTPUT || '.build/opnsense-review');
    await fs.mkdir(output,{recursive:true});
    const browser = await chromium.launch({executablePath:env.CHROMIUM_PATH,headless:true,args:['--no-sandbox']});
    const options = {ignoreHTTPSErrors:true,viewport:{width:1400,height:1000}};
    let context;
    try {
        const login = await browser.newContext(options);
        const auth = await login.newPage();
        await auth.goto(url.href);
        await auth.locator('[name=usernamefld]').fill(env.VEIL_OPNSENSE_USER || 'root');
        await auth.locator('[name=passwordfld]').fill(env.VEIL_OPNSENSE_PASSWORD);
        await auth.locator('[name=passwordfld]').press('Enter');
        await auth.waitForSelector('#veil-page');
        const state = await login.storageState();
        await login.close();
        context = await browser.newContext({...options,storageState:state,recordVideo:env.VEIL_OPNSENSE_VIDEO==='1'?{dir:output,size:options.viewport}:undefined});
        const page = await context.newPage();
        const errors=[];page.on('pageerror',error=>errors.push(error.message));
        await page.goto(url.href);
        await page.waitForFunction(()=>document.querySelector('#veil-state')?.textContent);
        const field = id=>page.locator('#veil-'+id);
        const badge = value=>page.waitForFunction(value=>document.querySelector('#veil-state').textContent===value,value);
        const pause = ()=>page.waitForTimeout(env.VEIL_OPNSENSE_VIDEO==='1'?900:50);
        assert.equal(await field('file').isVisible(),false);
        if (env.VEIL_OPNSENSE_READONLY==='1') {
            assert.equal(await field('settings').isVisible(),false);
            for (const id of ['start','stop','apply']) assert.equal(await field(id).isDisabled(),true);
            const allowed = await context.request.get(new URL('/api/veil/service/status',url).href,{maxRedirects:0});
            assert.equal(allowed.status(),200);
            assert.equal((await allowed.json()).version,1);
            const denied = await context.request.get(new URL('/api/veil/settings/get',url).href,{maxRedirects:0});
            // OPNsense may redirect a session-authenticated ACL denial before MVC dispatch.
            assert.ok([302,403].includes(denied.status()));
            if(denied.status()===302) assert.equal(denied.headers().location,'/');
            const mutation = await page.evaluate(async()=>{
                const before=await ajaxGet('/api/veil/service/status',{});
                const blocked=await new Promise(resolve=>ajaxCall('/api/veil/service/stop',{}).done(()=>resolve(false)).fail((xhr,reason)=>resolve(xhr.status===403 || (xhr.status===200 && reason==='parsererror'))));
                const after=await ajaxGet('/api/veil/service/status',{});
                return {blocked,unchanged:before.status.state===after.status.state};
            });
            assert.deepEqual(mutation,{blocked:true,unchanged:true});
            await page.screenshot({path:path.join(output,'readonly.png'),fullPage:true});
            assert.deepEqual(errors,[]);
            console.log('Read-only status UI and API denial passed');
            return;
        }
        if (await field('stop').isEnabled()) {await field('stop').click();await badge(labels.stopped);}
        await pause();
        const profile = JSON.parse(await fs.readFile(env.VEIL_OPNSENSE_CONFIG,'utf8'));
        assert.equal(profile.inbound,'mixed');
        await field('file').setInputFiles({name:'connection.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(profile))});
        await page.getByText(labels.imported,{exact:true}).waitFor();
        await field('enabled').setChecked(!(await field('enabled').isChecked()));
        await pause();
        await field('save').click();
        await page.getByText(labels.saved,{exact:true}).waitFor();
        await field('start').click();
        await badge(labels.running);await pause();
        await field('protocol').selectOption('http');
        await field('save').click();
        await page.waitForFunction(()=>!document.querySelector('#veil-apply').disabled);
        await field('apply').click();await pause();
        await page.getByRole('button',{name:labels.cancel,exact:true}).click();
        assert.equal(await field('state').textContent(),labels.running);
        await field('apply').click();
        await page.getByRole('button',{name:labels.apply,exact:true}).click();
        await page.waitForFunction(()=>document.querySelector('#veil-notice').hidden && document.querySelector('#veil-apply').disabled);
        await pause();
        await page.screenshot({path:path.join(output,'running.png'),fullPage:true});
        await field('stop').click();await badge(labels.stopped);await pause();
        // A stale tab must neither overwrite a later save nor apply an unseen revision.
        const conflicts = await page.evaluate(async()=>{
            const before=await ajaxGet('/api/veil/settings/get',{});
            const saved=await ajaxCall('/api/veil/settings/save',{profile:before.profile,enabled:before.enabled?'0':'1',expected_revision:before.revision});
            const stale=await ajaxCall('/api/veil/settings/save',{profile:before.profile,enabled:before.enabled?'1':'0',expected_revision:before.revision});
            const apply=await ajaxCall('/api/veil/service/restart',{expected_revision:before.revision});
            const after=await ajaxGet('/api/veil/settings/get',{});
            const status=await ajaxGet('/api/veil/service/status',{});
            return {saved:saved.saved,saveCode:stale.error?.code,applyCode:apply.error?.code,unchanged:after.revision===saved.revision,noConfig:status.config===undefined};
        });
        assert.deepEqual(conflicts,{saved:true,saveCode:'conflict',applyCode:'conflict',unchanged:true,noConfig:true});
        await field('file').setInputFiles({name:'invalid.json',mimeType:'application/json',buffer:Buffer.from('{bad')});
        await page.getByText(labels.invalid,{exact:true}).waitFor();await pause();
        await page.screenshot({path:path.join(output,'error.png'),fullPage:true});
        assert.deepEqual(errors,[]);
        const video=page.video();await context.close();context=null;
        if(video){await video.saveAs(path.join(output,'interaction.webm'));await video.delete();}
        const mobile=await browser.newContext({...options,storageState:state,viewport:{width:390,height:844}});
        const screen=await mobile.newPage();await screen.goto(url.href);await screen.waitForSelector('#veil-page');
        assert.equal(await screen.evaluate(()=>document.querySelector('#veil-page').getBoundingClientRect().right>innerWidth+1),false);
        await screen.screenshot({path:path.join(output,'mobile-status.png'),fullPage:false});
        await screen.locator('#veil-save').scrollIntoViewIfNeeded();
        await screen.screenshot({path:path.join(output,'mobile-config.png'),fullPage:false});
        await mobile.close();
        console.log(JSON.stringify({result:'passed',language:zh?'zh':'en',revisionConflicts:true,output}));
    } finally {if(context)await context.close();await browser.close();}
})().catch(error=>{console.error(error.message.split('Call log:')[0].trim());process.exitCode=1;});
