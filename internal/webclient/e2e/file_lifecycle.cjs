'use strict';
const {chromium}=require('playwright'),assert=require('node:assert/strict'),fs=require('node:fs/promises');
let browser,page,stage='start',safeHint='',responseCodes=[];
process.on('SIGINT',()=>{browser?.close().finally(()=>process.exit(130));});
(async()=>{
 const input=JSON.parse(await fs.readFile(process.env.WEB_FILE_INPUT,'utf8'));
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});
 try{
  const context=await browser.newContext({ignoreHTTPSErrors:true,acceptDownloads:true});page=await context.newPage();
  page.on('response',r=>{if(new URL(r.url()).pathname.startsWith('/api/v1/files/'))responseCodes.push({path:new URL(r.url()).pathname,status:r.status()});});
  let typed=[];page.on('response',async r=>{try{if(/\/messages\?/.test(r.url())&&r.url().includes('message_format=typed_v1')&&r.status()===200)typed.push(...(await r.json()).messages.filter(m=>m.message_type==='file'));}catch(_) {}});
  const login=async()=>{stage='PKCE';await page.goto(input.baseURL+'/web/');await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible',timeout:15000});assert.equal(new URL(page.url()).search,'');await page.locator('#identity-options .identity-button').first().click();await page.locator(input.kind==='group'?'.group-card':'.conversation-button').first().click();await page.locator('#file-controls').waitFor({state:'visible',timeout:15000});};
  await login();
  for(const sample of input.samples){
   stage='select';const buffer=await fs.readFile(sample.path);await page.locator('#file-select').setInputFiles({name:sample.name,mimeType:sample.mime,buffer});
   if(sample.outcome==='select'){await page.locator('#file-status').filter({hasText:'请选择名称有效'}).waitFor({timeout:10000});assert.equal(await page.locator('#file-upload').isDisabled(),true);continue;}
   await page.waitForFunction(()=>!document.getElementById('file-upload').disabled,{},{timeout:15000});
   stage='PUT';const uploadReply=page.waitForResponse(r=>new URL(r.url()).pathname.endsWith('/content')&&r.request().method()==='PUT',{timeout:15000});await page.locator('#file-upload').click();const response=await uploadReply;
   if(input.rejected){
    stage='reject';if(sample.outcome==='put'){assert.notEqual(response.status(),200);await page.waitForFunction(()=>document.getElementById('file-status').textContent.includes('结果待确认'),{},{timeout:10000});}
    else {assert.equal(response.status(),200);await page.waitForFunction(()=>/文件未通过扫描|扫描未完成/.test(document.getElementById('file-status').textContent),{},{timeout:20000});}
    assert.equal(await page.locator('#messages .file-message-card strong').count(),0);await page.locator('#file-cancel').click();continue;
   }
   assert.equal(response.status(),200);stage='actual scan';await page.waitForFunction(()=>document.getElementById('file-status').textContent.includes('扫描通过'),{},{timeout:20000});
   stage='send';await page.locator('#message-text').fill('文件说明');await page.locator('#send-button').click();await page.getByText(sample.name,{exact:true}).waitFor({timeout:15000});
   await page.waitForFunction(()=>document.getElementById('send-hint').textContent==='已保存',{},{timeout:10000});assert.equal(await page.getByText('已保存',{exact:true}).count(),1);
   stage='download';for(let attempt=0;attempt<3;attempt++){
    await page.getByRole('button',{name:'请求下载',exact:true}).last().click();
    await page.waitForFunction(()=>!!document.querySelector('[data-file-save]') || document.getElementById('file-save').textContent.includes('下载未完成'),{},{timeout:15000});
    if(await page.locator('[data-file-save]').count())break;
    const last=responseCodes.at(-1);assert([409,503].includes(last?.status));
    await page.waitForFunction(()=>![...document.querySelectorAll('#messages button')].at(-1)?.disabled);
   }
   await page.locator('[data-file-save]').waitFor({timeout:15000});
   assert.equal(await page.locator('[data-file-save]').count(),1);const event=page.waitForEvent('download');await page.locator('[data-file-save]').click();const saved=await event;assert.equal(saved.suggestedFilename(),sample.name);const savedPath=await saved.path();assert.equal(buffer.equals(await fs.readFile(savedPath)),true);await page.locator('[data-file-save]').waitFor({state:'detached'});
   await page.waitForFunction(()=>!document.getElementById('file-select').disabled);
  }
  if(!input.rejected){stage='typed projection';for(const sample of input.samples){const card=typed.find(m=>m.attachment?.original_filename===sample.name);assert(card);assert.equal(card.attachment.available,true);assert.equal(card.attachment.download_available,false);assert.equal(card.caption,'文件说明');assert(!JSON.stringify(card).includes('object_key'));}
   stage='fresh PKCE';await page.reload();await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible',timeout:15000});await page.locator('#identity-options .identity-button').first().click();await page.locator(input.kind==='group'?'.group-card':'.conversation-button').first().click();for(const sample of input.samples)await page.getByText(sample.name,{exact:true}).waitFor({timeout:15000});assert.equal(await page.locator('[data-file-save]').count(),0);
  }
  await context.close();console.log(JSON.stringify(input.rejected?{actual_scan_rejected:true,no_file_message:true,oversize_blocked:true}:{reserve_put_actual_scan:true,typed_send_pull:true,explicit_save_equal:true,Chinese_name:true,reload_PKCE:true,download_available_false:true}));
 }catch(e){safeHint=await page.locator('#file-status').textContent().catch(()=> 'unavailable');console.error(JSON.stringify({hint:safeHint,codes:responseCodes.slice(-8)}));throw e;}finally{await browser.close();}
})().catch(()=>{console.error('real file browser failed at '+stage);process.exitCode=1;});
