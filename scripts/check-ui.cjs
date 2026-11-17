const { chromium } = require('../web/node_modules/playwright');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const net = require('node:net');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');

const root = path.resolve(__dirname, '..');
const artifacts = path.join(root, '.dev', 'ui-check');
fs.mkdirSync(artifacts, {recursive:true});
const runtime = fs.mkdtempSync(path.join(require('node:os').tmpdir(), 'prtseek-ui-'));
const binary = path.resolve(process.argv[2] || './prtseek');
const report = { version: '', checks: [], colors: [], pageErrors: [], networkErrors: [], requests: [] };
const delay = ms => new Promise(r => setTimeout(r, ms));
function request(base, route, body, token) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? null : JSON.stringify(body);
    const req = http.request(base + route, { method: data ? 'POST' : 'GET', headers: {
      ...(data ? {'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data)} : {}),
      ...(token ? {Authorization: `Bearer ${token}`} : {})
    } }, res => {
      let value = ''; res.on('data', x => value += x);
      res.on('end', () => res.statusCode === 200 ? resolve(JSON.parse(value)) : reject(new Error(`${route}: ${res.statusCode} ${value}`)));
    });
    req.on('error', reject); if (data) req.write(data); req.end();
  });
}
(async () => {
  const sock = net.createServer();
  await new Promise(r => sock.listen(0, '127.0.0.1', r));
  const port = sock.address().port; await new Promise(r => sock.close(r));
  const base = `http://127.0.0.1:${port}`;
  const log = fs.openSync(path.join(runtime, 'server.log'), 'w');
  const processHandle = spawn(binary, ['-addr', `127.0.0.1:${port}`, '-proxy', '127.0.0.1:0', '-data', path.join(runtime, 'data')], {
    cwd: runtime, windowsHide: true, stdio: ['ignore', log, log],
    env: {...process.env, prtseek_CONFIG: path.join(runtime, 'missing.json'), prtseek_DB_PATH: path.join(runtime, 'state.db'), prtseek_SKILL_DIR: path.join(runtime, 'skills')}
  });
  let browser, page;
  try {
    for (let i=0; i<60; i++) {
      try { const health = await request(base, '/api/health'); report.version = health.version; break; } catch (e) { if(i===59) throw e; await delay(200); }
    }
    assert.equal(report.version, fs.readFileSync(path.join(root, 'VERSION'), 'utf8').trim());
    const password = crypto.randomBytes(20).toString('hex');
    const initialized = await request(base, '/api/auth/init', { password });
    const loggedIn = initialized.token ? initialized : await request(base, '/api/auth/login', {username:'prtseek', password});
    await request(base, '/api/conversations', {agent_key:'triage', title:'界面回归验证', mode:'ctf'}, loggedIn.token);
    browser = await chromium.launch({...(process.env.PRTSEEK_BROWSER_CHANNEL ? {channel:process.env.PRTSEEK_BROWSER_CHANNEL} : {}), headless:true});
    const context = await browser.newContext({viewport:{width:1440,height:960}, reducedMotion:'no-preference', colorScheme:'light'});
    await context.addInitScript(() => {
      window.__bootEvents = [];
      let playing = false;
      const observer = new MutationObserver(() => {
        const dialog = document.querySelector('.prts-boot[data-playing="true"]');
        if (dialog && !playing) { playing=true; window.__bootEvents.push({event:'start', at:performance.now(), animation:getComputedStyle(dialog).animationName, modal:dialog.matches(':modal')}); }
        if (!dialog && playing) { playing=false; window.__bootEvents.push({event:'end', at:performance.now()}); }
      });
      observer.observe(document, {subtree:true, childList:true, attributes:true, attributeFilter:['data-playing']});
    });
    page = await context.newPage();
    page.setDefaultTimeout(10000);
    page.on('pageerror', err => report.pageErrors.push(err.message));
    page.on('request', req => {if(req.method()==='POST') report.requests.push({method:req.method(),path:new URL(req.url()).pathname});});
    page.on('response', res => {if(res.status()>=400) report.networkErrors.push(`${res.status()} ${new URL(res.url()).pathname}`);});
    const boot = page.locator('.prts-boot[data-playing="true"]');
    async function bootEnds(label) {
      await page.waitForFunction(() => window.__bootEvents?.some(e => e.event==='start'));
      await page.locator('.prts-boot').waitFor({state:'detached',timeout:5000});
      const events = await page.evaluate(() => window.__bootEvents);
      assert.equal(events[0].modal,true);
      report.checks.push({name:label, events});
    }
    await page.goto(base+'/login/', {waitUntil:'domcontentloaded'});
    await boot.waitFor();
    await page.evaluate(() => document.querySelector('.prts-boot').getAnimations({subtree:true}).forEach(a => {a.pause();a.currentTime=400;}));
    await page.screenshot({path:path.join(artifacts,'boot-emblem.png')});
    await page.evaluate(() => document.querySelector('.prts-boot')?.getAnimations({subtree:true}).forEach(a => {a.currentTime=1450;}));
    await page.screenshot({path:path.join(artifacts,'boot-wordmark.png')});
    await bootEnds('login page boot and timer dismissal');
    async function login(label) {
      await page.locator('#password').fill(password);
      await page.locator('#agree-terms').click();
      await page.evaluate(() => {window.__formEvents=[]; for(const kind of ['submit','invalid','click']) document.querySelector('form').addEventListener(kind,e=>window.__formEvents.push({kind,tag:e.target.tagName,type:e.target.type}),true);});
      await Promise.all([page.waitForURL(url=>url.pathname.replace(/\/$/,'')==='/chat',{waitUntil:'domcontentloaded'}),page.locator('button[type="submit"]').click()]);
      await bootEnds(label);
      await page.locator('.conversation-sidebar').waitFor();
    }
    await login('first login boot');
    await page.getByRole('button',{name:/^界面回归验证 triage/}).click();
    await page.locator('textarea').waitFor();
    await page.evaluate(() => {document.documentElement.classList.remove('dark');document.documentElement.dataset.themeMode='light';});
    async function colors(label) {
      await page.waitForTimeout(200); // Let theme color transitions settle before capture.
      const values = await page.evaluate(() => {
        const selectors = ['.conversation-sidebar','.prts-chat-workspace','.conversation-main','[class*="_composer"]','textarea'];
        if (document.querySelector('[data-slot="sidebar-inner"]')) selectors.unshift('[data-slot="sidebar-inner"]');
        if (document.querySelector('[data-slot="sidebar"][data-mobile="true"]')) selectors.unshift('[data-slot="sidebar"][data-mobile="true"]');
        const canvas=document.createElement('canvas'), ctx=canvas.getContext('2d');
        function canonical(v) {ctx.fillStyle=v;ctx.fillRect(0,0,1,1);return Array.from(ctx.getImageData(0,0,1,1).data).join(',');}
        function effective(el) { let v; while(el) {v=getComputedStyle(el).backgroundColor;if(v!=='rgba(0, 0, 0, 0)'&&v!=='transparent')return canonical(v);el=el.parentElement;}return v; }
        return selectors.map(selector => {const el=document.querySelector(selector); return {selector, exists:!!el, background:el ? effective(el):null};});
      });
      assert(values.every(v=>v.exists), JSON.stringify(values));
      if(label.endsWith('desktop')) assert(values.some(v=>v.selector==='[data-slot="sidebar-inner"]'));
      assert(values.every(v=>v.background===values[0].background), JSON.stringify(values));
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      assert.equal(overflow,false); report.colors.push({label,values,overflow});
      await page.screenshot({path:path.join(artifacts,`${label}.png`),fullPage:true});
    }
    await colors('chat-light-desktop');
    // Updating the server must preserve an unsent draft until explicit reload.
    const healthBefore = await request(base, '/api/health');
    assert.equal(healthBefore.build_id.length, 64);
    await page.locator('textarea').fill('unsent upgrade regression draft');
    await page.route('**/api/health', route => route.fulfill({json:{...healthBefore,build_id:'new-fixture-build'}}));
    await page.evaluate(() => window.dispatchEvent(new Event('focus')));
    await page.getByTestId('build-update-notice').waitFor();
    assert.equal(await page.locator('textarea').inputValue(), 'unsent upgrade regression draft');
    await page.unroute('**/api/health');
    await page.getByRole('button',{name:'刷新加载新版'}).click();
    await page.waitForLoadState('domcontentloaded');
    await page.locator('.prts-boot').waitFor({state:'detached',timeout:5000});
    assert.equal(await page.getByTestId('build-update-notice').count(),0);
    report.checks.push({name:'server update notice preserves draft and refresh loads current build',passed:true});
    await page.locator('[data-sidebar="footer"] button').click();
    await Promise.all([page.waitForURL(/\/login\/?$/,{waitUntil:'domcontentloaded'}),page.getByRole('menuitem',{name:'退出登录'}).click()]);
    await bootEnds('logout boot');
    await login('second login boot');
    await page.evaluate(() => {window.__navigationProof='same-document';});
    await page.locator('a[href^="/settings"]').click();
    await page.waitForURL(url=>url.pathname.replace(/\/$/,'')==='/settings');
    await page.getByRole('link',{name:'对话与会话',exact:true}).click();
    await page.waitForURL(url=>url.pathname.replace(/\/$/,'')==='/chat');
    await page.locator('textarea').waitFor();
    assert.equal(await page.evaluate(()=>window.__navigationProof),'same-document');
    assert.equal(await page.locator('.prts-boot').count(),0);
    report.checks.push({name:'settings navigation and return without reload',passed:true});
    await page.evaluate(() => {document.documentElement.classList.add('dark');document.documentElement.dataset.themeMode='dark';});
    await colors('chat-dark-desktop');
    await page.evaluate(() => {document.documentElement.classList.remove('dark');document.documentElement.dataset.themeMode='light';});
    await page.setViewportSize({width:390,height:844});
    await colors('chat-light-mobile');
    await page.locator('[data-slot="sidebar-trigger"]').click();
    await page.locator('[data-slot="sidebar"][data-mobile="true"]').waitFor();
    await colors('chat-light-mobile-navigation');
    await page.keyboard.press('Escape');
    await page.locator('[data-slot="sidebar"][data-mobile="true"]').waitFor({state:'detached'});
    await page.getByRole('button',{name:'会话管理',exact:true}).click();
    await page.locator('.conversation-sidebar').waitFor();
    await page.screenshot({path:path.join(artifacts,'chat-mobile-history.png'),fullPage:true});
    report.checks.push({name:'mobile history toggle',passed:true});
    await page.reload({waitUntil:'domcontentloaded'}); await boot.waitFor();
    await page.keyboard.press('Escape'); await page.locator('.prts-boot').waitFor({state:'detached',timeout:1000});
    report.checks.push({name:'Escape dismissal',passed:true});
    await page.reload({waitUntil:'domcontentloaded'}); await boot.waitFor();
    await page.getByRole('button',{name:'跳过 / SKIP'}).click(); await page.locator('.prts-boot').waitFor({state:'detached',timeout:1000});
    report.checks.push({name:'skip dismissal',passed:true});
    await page.emulateMedia({reducedMotion:'reduce'});
    await page.reload({waitUntil:'domcontentloaded'}); await boot.waitFor();
    assert.equal(await boot.evaluate(el=>getComputedStyle(el).animationName),'none');
    await bootEnds('reduced motion static boot');
    const reduced = report.checks.at(-1).events;
    assert(reduced.at(-1).at-reduced[0].at<1500);
    assert.deepEqual(report.pageErrors,[]); assert.deepEqual(report.networkErrors,[]);
    report.passed=true;
    console.log(JSON.stringify(report,null,2));
  } catch(e) {report.passed=false; report.error=e.stack; if(page) {report.url=page.url();report.form=await page.evaluate(()=>({events:window.__formEvents,button:document.querySelector('button[type=submit]')?.outerHTML,modal:!!document.querySelector(':modal')})).catch(()=>null);report.body=await page.locator('body').innerText().catch(()=>null);await page.screenshot({path:path.join(artifacts,'failure.png')}).catch(()=>{});} console.error(e);} finally {
    fs.writeFileSync(path.join(artifacts,'result.json'),JSON.stringify(report,null,2));
    if(browser) await browser.close();
    if(processHandle.exitCode===null) {const exited=new Promise(r=>processHandle.once('exit',r));processHandle.kill();await exited;} fs.closeSync(log);
    fs.rmSync(runtime,{recursive:true,force:true});
  }
  if(!report.passed) process.exitCode=1;
})().catch(e=>{console.error(e);process.exitCode=1;});
