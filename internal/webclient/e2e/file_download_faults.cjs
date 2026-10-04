 'use strict';
const {chromium}=require('playwright'),assert=require('node:assert/strict'),fs=require('node:fs/promises');let browser,stage='start',safeStatuses=[];process.on('SIGINT',()=>browser?.close().finally(()=>process.exit(130)));
(async()=>{const input=JSON.parse(await fs.readFile(process.env.WEB_FILE_INPUT,'utf8'));browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});try{const context=await browser.newContext({ignoreHTTPSErrors:true}),page=await context.newPage();await page.addInitScript(()=>{window.blobCalls=0;const original=URL.createObjectURL;URL.createObjectURL=function(...args){window.blobCalls++;return original.apply(this,args);};});stage='PKCE';await page.goto(input.baseURL+'/web/');await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible'});await page.locator('#identity-options .identity-button').first().click();await page.locator('.conversation-button').first().click();await page.locator('#messages').getByText(input.name,{exact:true}).waitFor();const statuses=safeStatuses;page.on('response',r=>{if(new URL(r.url()).pathname.endsWith('/content'))statuses.push(r.status());});
 for(let i=0;i<4;i++){
 stage=['actual 200 cut','actual length lie','redirect rejected','oversized header'][i];
 for(let attempt=0;attempt<3;attempt++){
 await page.waitForFunction(async()=>{const r=await fetch('/__p425/download-idle',{cache:'no-store'});return (await r.json()).idle;});
 const before=statuses.length;await page.getByRole('button',{name:'请求下载',exact:true}).click();await page.waitForFunction(()=>document.getElementById('file-save').textContent.includes('下载未完成'));await page.waitForFunction(()=>![...document.querySelectorAll('#messages button')].at(-1).disabled);
 const observed=await context.request.get(input.baseURL+'/__p425/download-fault-state');const count=(await observed.json()).count;if(count===i+1)break;assert.equal(count,i);assert(statuses.length>before && [409,503].includes(statuses.at(-1)));
 }
 const state=await context.request.get(input.baseURL+'/__p425/download-fault-state');assert.equal((await state.json()).count,i+1);
 assert.equal(await page.locator('[data-file-save]').count(),0);assert.equal(await page.evaluate(()=>window.blobCalls),0);
 }
 assert(statuses.includes(200));await context.close();console.log(JSON.stringify({actual_200_cut_no_blob:true,length_lie_no_blob:true,redirect_no_blob:true,oversize_no_blob:true}));}finally{await browser.close();}})().catch(e=>{console.error(JSON.stringify({stage,kind:e.name,code:e.code,statuses:safeStatuses,line:(e.stack||'').split('\n').find(s=>s.includes('file_download_faults.cjs:'))?.trim()}));console.error('real download fault browser failed at '+stage);process.exitCode=1;});
