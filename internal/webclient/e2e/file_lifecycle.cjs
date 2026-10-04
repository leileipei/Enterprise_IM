'use strict';
const {waitPageFlag}=require("./file_browser_poll.cjs");
const {chromium}=require('playwright'),assert=require('node:assert/strict'),fs=require('node:fs/promises');
let browser,page,stage='start',safeHint='',responseCodes=[];
process.on('SIGINT',()=>{browser?.close().finally(()=>process.exit(130));});
(async()=>{
 const input=JSON.parse(await fs.readFile(process.env.WEB_FILE_INPUT,'utf8'));
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});
 try{
  const context=await browser.newContext({ignoreHTTPSErrors:true,acceptDownloads:true});page=await context.newPage();
  page.on('response',r=>{if(new URL(r.url()).pathname.startsWith('/api/v1/files/') || new URL(r.url()).pathname.endsWith('/messages'))responseCodes.push({path:new URL(r.url()).pathname,status:r.status()});});
  let syncCount=0,pullsAfterSync=0,resolveSync;const actualSync=new Promise(resolve=>resolveSync=resolve);page.on('websocket',socket=>socket.on('framereceived',event=>{const frame=JSON.parse(String(event.payload));if(frame.type==='sync_required'){assert.deepEqual(frame,{type:'sync_required'});syncCount++;resolveSync();}}));page.on('request',r=>{if(syncCount && r.method()==='GET' && r.url().includes('message_format=typed_v1'))pullsAfterSync++;});
  let typed=[];page.on('response',async r=>{try{if(/\/messages\?/.test(r.url())&&r.url().includes('message_format=typed_v1')&&r.status()===200)typed.push(...(await r.json()).messages.filter(m=>m.message_type==='file'));}catch(_) {}});
  const login=async()=>{stage='PKCE';await page.goto(input.baseURL+'/web/');await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible',timeout:15000});assert.equal(new URL(page.url()).search,'');await page.locator('#identity-options .identity-button').first().click();await page.locator(input.kind==='group'?'.group-card':'.conversation-button').first().click();await page.locator('#file-controls').waitFor({state:'visible',timeout:15000});await page.locator('#connection-state').filter({hasText:'实时通知已连接'}).waitFor({timeout:15000});};
  let pullArmed=false,pullFaults=0;
  if(input.ackPullFault)await page.route("**/api/v1/conversations/*/messages?**",async route=>{if(pullArmed && pullFaults===0 && route.request().url().includes("message_format=typed_v1")){pullFaults++;const response=await route.fetch();assert.equal(response.status(),200);await route.abort("failed");}else await route.continue();});
  let groupArmed=false,groupHeld=false,releaseGroup,heldGroupResolve;const heldGroup=new Promise(r=>heldGroupResolve=r);const holdGroup=new Promise(r=>releaseGroup=r);
  if(input.groupComposer)await page.route('**/api/v1/groups?**',async route=>{if(!groupArmed || groupHeld)return route.continue();groupHeld=true;const response=await route.fetch();assert.equal(response.status(),200);heldGroupResolve();await holdGroup;await route.fulfill({response});});
  const reserves=[],puts=[],sends=[];
  if(input.unknown){
   await page.route('**/api/v1/conversations/*/files',async route=>{if(route.request().method()!=='POST')return route.continue();reserves.push(JSON.parse(route.request().postData()));const response=await route.fetch();assert([200,201].includes(response.status()));if(reserves.length===1)await route.abort('failed');else await route.fulfill({response});});
   await page.route('**/api/v1/files/*/content',async route=>{if(route.request().method()!=='PUT')return route.continue();puts.push(route.request().url());const response=await route.fetch();assert.equal(response.status(),200);await route.abort('failed');});
   await page.route('**/api/v1/*/*/messages',async route=>{if(route.request().method()!=='POST')return route.continue();sends.push(JSON.parse(route.request().postData()));const response=await route.fetch();assert.equal(response.status(),200);if(sends.length===1)await route.abort('failed');else{assert.equal((await response.json()).duplicate,true);await route.fulfill({response});}});
  }
  await login();
  for(const sample of input.samples){
   stage='select';const buffer=await fs.readFile(sample.path);await page.locator('#file-select').setInputFiles({name:sample.name,mimeType:sample.mime,buffer});
   if(sample.outcome==='select'){await page.locator('#file-status').filter({hasText:'请选择名称有效'}).waitFor({timeout:10000});assert.equal(await page.locator('#file-upload').isDisabled(),true);continue;}
   await page.waitForFunction(()=>!document.getElementById('file-upload').disabled,{},{timeout:15000});
   stage='PUT';if(input.unknown){await page.locator('#file-upload').click();await page.waitForFunction(()=>document.getElementById('file-status').textContent.includes('结果待确认'));assert.equal(reserves.length,1);await page.locator('#file-original-retry').click();await page.waitForFunction(()=>!document.getElementById('file-status-query').disabled && document.getElementById('file-status').textContent.includes('结果待确认'));assert.equal(puts.length,1);assert.deepEqual(reserves[0],reserves[1]);await page.locator('#file-status-query').click();}const uploadReply=input.unknown?Promise.resolve({status:()=>200}):page.waitForResponse(r=>new URL(r.url()).pathname.endsWith('/content')&&r.request().method()==='PUT',{timeout:15000});if(!input.unknown)await page.locator('#file-upload').click();const response=await uploadReply;
   if(input.rejected){
    stage='reject';if(sample.outcome==='put'){assert.notEqual(response.status(),200);await page.waitForFunction(()=>document.getElementById('file-status').textContent.includes('结果待确认'),{},{timeout:10000});}
    else {assert.equal(response.status(),200);await page.waitForFunction(()=>/文件未通过扫描|扫描未完成/.test(document.getElementById('file-status').textContent),{},{timeout:20000});}
    assert.equal(await page.locator('#messages .file-message-card strong').count(),0);await page.locator('#file-cancel').click();continue;
   }
   assert.equal(response.status(),200);stage='actual scan';await page.waitForFunction(()=>document.getElementById('file-status').textContent.includes('扫描通过'),{},{timeout:20000});
   stage='send';if(input.ackPullFault)pullArmed=true;if(input.groupComposer)groupArmed=true;await page.locator('#message-text').fill('文件说明');await page.locator('#send-button').click();if(input.unknown){await page.waitForFunction(()=>document.getElementById('send-hint').textContent.includes('结果待确认'));assert.equal(sends.length,1);if(input.groupComposer){await Promise.race([heldGroup,new Promise((_,reject)=>setTimeout(()=>reject(new Error('real group refresh absent')),15000))]);const groupResponse=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/v1/groups'&&r.request().method()==='GET');releaseGroup();await (await groupResponse).finished();await page.evaluate(()=>new Promise(requestAnimationFrame));stage='group frozen UI after refresh';assert.equal(await page.locator('#message-text').getAttribute('readonly')!==null,true);assert.equal(await page.locator('#discard-pending').isVisible(),true);}await page.locator('#send-button').click();await page.waitForFunction(()=>document.getElementById('send-hint').textContent==='已保存');assert.deepEqual(sends[0],sends[1]);assert.equal(puts.length,1);}await page.getByText(sample.name,{exact:true}).waitFor({timeout:15000});
   await page.waitForFunction(()=>document.getElementById('send-hint').textContent==='已保存',{},{timeout:10000});assert.equal(await page.getByText('已保存',{exact:true}).count(),1);
   if(input.groupSavedRefresh){stage='saved group refresh';const refresh=await page.waitForResponse(r=>new URL(r.url()).pathname==='/api/v1/groups'&&r.request().method()==='GET',{timeout:45000});await refresh.finished();await page.evaluate(()=>new Promise(requestAnimationFrame));assert.equal(await page.locator('#send-hint').textContent(),'已保存');}
   stage='download';for(let attempt=0;attempt<3;attempt++){
    await page.waitForLoadState('networkidle',{timeout:15000});await waitPageFlag(page,"/__p425/download-idle","idle",65000);
    const card=page.locator('.file-message-card').filter({has:page.getByText(sample.name,{exact:true})});const fileID=await card.getAttribute('data-file-id');const actualDownload=page.waitForResponse(r=>new URL(r.url()).pathname===`/api/v1/files/${fileID}/content` && r.request().method()==='GET');await card.getByRole('button',{name:'请求下载',exact:true}).click();const actualResponse=await actualDownload;
    await page.waitForFunction(()=>!!document.querySelector('[data-file-save]') || document.getElementById('file-save').textContent.includes('下载未完成'),{},{timeout:15000});
    if(await page.locator('[data-file-save]').count())break;
    assert([409,503].includes(actualResponse.status()));
    await page.waitForFunction(()=>![...document.querySelectorAll('#messages button')].at(-1)?.disabled);
   }
   await page.locator('[data-file-save]').waitFor({timeout:15000});
   assert.equal(await page.locator('[data-file-save]').count(),1);const event=page.waitForEvent('download');await page.locator('[data-file-save]').click();const saved=await event;assert.equal(saved.suggestedFilename(),sample.name);const savedPath=await saved.path();assert.equal(buffer.equals(await fs.readFile(savedPath)),true);await page.locator('[data-file-save]').waitFor({state:'detached'});
   await page.waitForFunction(()=>!document.getElementById('file-select').disabled);
  }
  if(!input.rejected){stage='actual Redis notification';await Promise.race([actualSync,new Promise((_,reject)=>setTimeout(()=>reject(new Error('real notification absent')),15000))]);assert(syncCount>0);assert(pullsAfterSync>0);if(input.ackPullFault)assert.equal(pullFaults,1);stage='typed projection';for(const sample of input.samples){const card=typed.find(m=>m.attachment?.original_filename===sample.name);assert(card);assert.equal(card.attachment.available,true);assert.equal(card.attachment.download_available,false);assert.equal(card.caption,'文件说明');assert(!JSON.stringify(card).includes('object_key'));}
   stage='fresh PKCE';await page.reload();await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible',timeout:15000});await page.locator('#identity-options .identity-button').first().click();await page.locator(input.kind==='group'?'.group-card':'.conversation-button').first().click();for(const sample of input.samples)await page.getByText(sample.name,{exact:true}).waitFor({timeout:15000});assert.equal(await page.locator('[data-file-save]').count(),0);
  }
  await context.close();console.log(JSON.stringify(input.rejected?{actual_scan_rejected:true,no_file_message:true,oversize_blocked:true}:{reserve_put_actual_scan:true,real_Redis_notification_pull:true,typed_send_pull:true,explicit_save_equal:true,Chinese_name:true,reload_PKCE:true,download_available_false:true}));
 }catch(e){safeHint=await page.locator('#file-status').textContent().catch(()=> 'unavailable');console.error(JSON.stringify({hint:safeHint,codes:responseCodes.slice(-12),kind:e.name,code:e.code,line:(e.stack||'').split('\n').find(s=>s.includes('file_lifecycle.cjs:'))?.trim()}));throw e;}finally{await browser.close();}
})().catch(()=>{console.error('real file browser failed at '+stage);process.exitCode=1;});
