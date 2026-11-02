'use strict';
// Loads the real HTTPS management origin. API fixtures are intercepted in-browser;
// no requests can write to the shared development registry or Host configuration.
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const base=process.env.GATEWAY_UI_URL||'https://gateway-dev.deeler.cn';
const out=process.env.GATEWAY_UI_OUTPUT||'/tmp';
(async()=>{
 const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_BIN,args:['--no-sandbox','--disable-dev-shm-usage']});
 try{
 const page=await browser.newPage({viewport:{width:1280,height:900}}),errors=[],writes=[];
 page.on('pageerror',error=>errors.push(error.message));
 const draft={id:'ui-fixture',domains:['ui-fixture.example.test'],enabled:true,https:{mode:'off'},acl:[],limits:[],proxies:[],rewrites:[],statics:{},affinityRules:[]};
 let record={draft,current:structuredClone(draft),revision:1,version:1,history:[]};
 await page.route(base+'/**',async route=>{
  const request=route.request(),url=new URL(request.url());
  if(url.pathname.startsWith('/api/')){
   if(request.method()!=='GET')writes.push(url.pathname);
   let data;
   switch(url.pathname){
    case '/api/session':data={csrf:'fixture-token'};break;
    case '/api/hosts':data=[record];break;
    case '/api/nodes':case '/api/services':data=[];break;
    case '/api/monitor-status':data={};break;
    case '/api/trash':data=[];break;
    case '/api/draft':record={...record,draft:request.postDataJSON().host,revision:record.revision+1};data=record;break;
    default:throw Error('Unexpected fixture API: '+url.pathname);
   }
   return route.fulfill({json:data});
  }
  if(process.env.GATEWAY_UI_SOURCE){
   const name=url.pathname==='/'?'index.html':url.pathname.slice(1);
   if(['index.html','app.js','acl.js','rule-editor.js','monitor.js','style.css'].includes(name))return route.fulfill({path:path.join(process.env.GATEWAY_UI_SOURCE,name),contentType:name.endsWith('.js')?'text/javascript':name.endsWith('.css')?'text/css':'text/html'});
  }
  return route.continue();
 });
 const pass=name=>console.log('PASS '+name);
 const tab=async name=>page.locator(`[data-tab="${name}"]`).click();
 const confirm=async()=>{await page.locator('#ruleEditorForm button[type=submit]').click();await page.locator('#ruleEditor').waitFor({state:'hidden'})};
 await page.goto(base);await page.locator('[data-edit="ui-fixture"]').click();await tab('acl');
 assert.equal(await page.locator('#addAdminACL').count(),0);
 assert.equal(await page.locator('#deleteHost').isVisible(),false);
 await page.locator('#addACL').click();
 assert.equal(await page.locator('[data-acl-condition][data-field=value]').inputValue(),'');
 await page.locator('#ruleEditorForm button[type=submit]').click();await page.locator('#ruleEditorError').filter({hasText:'匹配值'}).waitFor();
 await page.locator('[data-acl-condition][data-field=value]').fill('/private/');
 await page.locator('[data-acl-ips]').fill('192.0.2.1\n198.51.100.0/24');
 // Structural edits must not submit the modal or discard pending batch IP text.
 await page.locator('[data-acl-add-condition][data-kind=scope]').click();
 assert.equal(await page.locator('#ruleEditor').isVisible(),true);
 assert.equal(await page.locator('[data-acl-ips]').inputValue(),'192.0.2.1\n198.51.100.0/24');
 await page.locator('[data-acl-delete-condition]').last().click();await confirm();
 assert.equal(await page.locator('#aclRows input,#aclRows textarea,#aclRows select').count(),0);
 assert.match(await page.locator('#aclRows').innerText(),/192\.0\.2\.1/);
 await page.locator('#aclRows [data-edit-rule]').click();
 await page.locator('[data-acl-condition][data-field=value]').fill('/discard/');await page.keyboard.press('Escape');
 assert.match(await page.locator('#aclRows').innerText(),/\/private\//);
 await page.locator('#aclRows [data-edit-rule]').click();
 await page.locator('[data-acl-condition][data-field=value]').fill('/edited/');await confirm();
 await page.locator('#addACL').click();await page.locator('#cancelRuleEditor').click();
 assert.equal(await page.locator('#aclRows [data-edit-rule]').count(),1);
 pass('ACL neutral defaults; batch IPs; structural editing; cancel/Escape; confirmed edits; summary-only list');
 await page.locator('#aclRows [data-delete-rule]').click();await page.locator('#cancelConfirm').click();
 assert.equal(await page.locator('#aclRows [data-edit-rule]').count(),1);
 await page.locator('#aclRows [data-delete-rule]').click();
 assert.match(await page.locator('#confirmDiff').innerText(),/不删除 Host/);
 await page.locator('#acceptConfirm').click();assert.equal(await page.locator('#aclRows [data-edit-rule]').count(),0);
 pass('Rule deletion confirmation only removes the rule');
 // Add a final ACL for save/reopen, then exercise sibling modules.
 await page.locator('#addACL').click();await page.locator('[data-acl-condition][data-field=value]').fill('/saved/');await confirm();
 for(const [name,target] of [['proxy','backend'],['rewrite','/new'],['redirect','https://example.test/'],['static','www/']]){
  await tab(name);await page.locator('#addRoute').click();await page.locator('[data-route][data-field=to]').fill(target);await confirm();
  assert.equal(await page.locator('#routeRows [data-edit-rule]').count(),1);
  assert.equal(await page.locator('#routeRows input').count(),0);
 }
 await tab('limits');await page.locator('#addLimit').click();await page.locator('[data-count-mode]').selectOption('custom');
 await page.locator('[data-dimension][data-field=source]').selectOption('ip');
 await page.locator('[data-add-condition]').click();assert.equal(await page.locator('#ruleEditor').isVisible(),true);
 await page.locator('[data-condition][data-field=value]').fill('/api/');await confirm();
 await tab('affinity');await page.locator('#addAffinity').click();await page.locator('[data-affinity][data-field=path]').fill('/chat/*');await confirm();
 pass('Proxy/rewrite/redirect/static/limit/affinity use independent modal drafts');
 await page.locator('#saveDraft').click();await page.locator('#toast').filter({hasText:'草稿已保存'}).waitFor();
 await page.locator('#closeEditor').click();await page.locator('[data-edit="ui-fixture"]').click();await tab('acl');
 assert.match(await page.locator('#aclRows').innerText(),/\/saved\//);
 assert.equal(record.draft.proxies.length,1);assert.equal(record.draft.rewrites.length,2);assert.equal(record.draft.limits.length,1);assert.equal(record.draft.affinityRules.length,1);
 assert.equal(await page.locator('#deleteHost').isVisible(),false);
 await tab('config');assert.equal(await page.locator('#deleteHost').innerText(),'移入回收站');
 pass('Save/reopen retains all rule categories; Host deletion is confined to configuration');
 await tab('acl');await page.locator('#aclRows [data-edit-rule]').click();
 await page.setViewportSize({width:390,height:844});
 await page.screenshot({path:path.join(out,'acl-dialog-mobile.png'),fullPage:true});
 const layout=await page.locator('#ruleEditor').evaluate(el=>{const r=el.getBoundingClientRect();return {left:r.left,right:r.right,width:innerWidth,overflow:el.scrollWidth>el.clientWidth+1}});
 assert.ok(layout.left>=0&&layout.right<=layout.width&&!layout.overflow,JSON.stringify(layout));
 await page.locator('#ruleEditorForm button[type=submit]').scrollIntoViewIfNeeded();await confirm();
 await page.screenshot({path:path.join(out,'acl-list-mobile.png'),fullPage:true});
 assert.deepEqual(errors,[]);assert.deepEqual(writes,['/api/draft']);
 await page.locator('#closeEditor').click();await page.locator('#openTrash').click();await page.locator('#trashRows .empty').waitFor();await page.screenshot({path:path.join(out,'trash-mobile.png'),fullPage:true});await page.locator('#doneTrash').click();
 pass('390px layout and modal footer reachable; no page errors; all API writes intercepted');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exitCode=1});
