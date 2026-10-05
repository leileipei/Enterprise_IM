'use strict';
// Input is private fixture metadata on stdin. Legacy browser assertions run
// against the official API; only transport failures/waits are adapted here.
const fs=require('node:fs/promises'),path=require('node:path'),{spawn}=require('node:child_process');
(async()=>{
 let raw='';for await(const chunk of process.stdin)raw+=chunk;
 const input=JSON.parse(raw),scenario=input.scenario;
 const waitIdle=async()=>{const deadline=Date.now()+65000;while(Date.now()<deadline){try{if(JSON.parse(await fs.readFile(input.observationFile,'utf8')).idle===true)return;}catch(_){}await new Promise(r=>setTimeout(r,50));}throw Error('actual session settlement did not reach idle');};
 if(scenario==='existing_download'||scenario==='existing_faults'){
  const {chromium}=require('playwright'),assert=require('node:assert/strict');const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});
  try{const context=await browser.newContext({ignoreHTTPSErrors:true,acceptDownloads:true}),page=await context.newPage();await page.addInitScript(()=>{window.blobCalls=0;const original=URL.createObjectURL;URL.createObjectURL=function(...args){window.blobCalls++;return original.apply(this,args);};});await page.goto(input.baseURL+'/web/');await page.getByRole('button',{name:/使用企业账号登录/}).click();await page.locator('#workspace').waitFor({state:'visible'});await page.locator('#identity-options .identity-button').first().click();await page.locator('.conversation-button').first().click();await page.getByText(input.name,{exact:true}).waitFor();
   if(scenario==='existing_download'){await waitIdle();const started=Date.now();await page.getByRole('button',{name:'请求下载',exact:true}).first().click();await page.locator('[data-file-save]').waitFor({timeout:65000});assert(Date.now()-started>10000 && Date.now()-started<60000);const event=page.waitForEvent('download');await page.locator('[data-file-save]').click();const saved=await event,actual=await fs.readFile(await saved.path()),expected=await fs.readFile(input.expectedPath);assert(actual.equals(expected));const digest=require('node:crypto').createHash('sha256').update(actual).digest('hex');await fs.writeFile(path.join(input.evidenceDir,'long-save.json'),JSON.stringify({source_sha256:digest,chrome_saved_sha256:digest,bytes:actual.length}),{mode:0o600});console.log(JSON.stringify({official_download_after_10s:true,within_original_60s:true,explicit_save_equal:true}));}
   else{for(const fault of ['compressed','slow-read']){await waitIdle();await page.route('**/api/v1/files/*/content',async route=>route.continue({headers:{...await route.request().allHeaders(),'X-P426-Transport-Fault':fault}}));const began=Date.now();await page.getByRole('button',{name:'请求下载',exact:true}).first().click();await page.locator('#file-save').filter({hasText:'下载未完成'}).waitFor({timeout:70000});assert.equal(await page.locator('[data-file-save]').count(),0);assert.equal(await page.evaluate(()=>window.blobCalls),0);if(fault==='slow-read')assert(Date.now()-began>=60000 && Date.now()-began<70000);await page.unroute('**/api/v1/files/*/content');}console.log(JSON.stringify({actual_compressed_no_blob:true,original_65s_timeout_no_blob:true}));}
   await context.close();
  }finally{await browser.close();}return;
 }
 const names={lifecycle:'file_lifecycle',unknown:'file_lifecycle',context:'file_context',settings:'file_settings',search:'file_search',faults:'file_download_faults'};
 if(!names[scenario])throw Error('unknown mandatory browser scenario');
 let source=await fs.readFile(path.join(__dirname,names[scenario]+'.cjs'),'utf8');
 source=source.replace('require("./file_browser_poll.cjs")',`require(${JSON.stringify(path.join(__dirname,'file_browser_poll.cjs'))})`);
 // The official audit-only process settles sessions. Retry by the explicit UI
 // action only after an actual 409/503, inside the original 65s operator budget.
 source=source.replaceAll('await waitPageFlag(page,"/__p425/download-idle","idle",65000);','await runtimeIdle();');
 source=source.replaceAll('await waitPageFlag(page,"/__p425/download-idle","idle");','await runtimeIdle();');
 source="const runtimeIdle=async()=>{const deadline=Date.now()+65000;while(Date.now()<deadline){try{if(JSON.parse(await require('node:fs/promises').readFile(JSON.parse(await require('node:fs/promises').readFile(process.env.WEB_FILE_INPUT,'utf8')).observationFile,'utf8')).idle===true)return;}catch(_){}await new Promise(r=>setTimeout(r,50));}throw Error('actual session settlement did not reach idle');};\n"+source;
 source=source.replaceAll('attempt<3','attempt<10');
 if(scenario==='lifecycle'||scenario==='unknown')source=source.replace("assert([409,503].includes(actualResponse.status()));","assert([409,503].includes(actualResponse.status()));await page.waitForTimeout(200);");
 if(scenario==='lifecycle'||scenario==='unknown')source=source.replace('const savedPath=await saved.path();assert.equal(buffer.equals(await fs.readFile(savedPath)),true);',`const savedPath=await saved.path();const savedBytes=await fs.readFile(savedPath);assert.equal(buffer.equals(savedBytes),true);const sourceHash=require('node:crypto').createHash('sha256').update(buffer).digest('hex'),savedHash=require('node:crypto').createHash('sha256').update(savedBytes).digest('hex');assert.equal(savedHash,sourceHash);await fs.writeFile(require('node:path').join(input.evidenceDir,'saved-'+sourceHash+'.json'),JSON.stringify({source_sha256:sourceHash,chrome_saved_sha256:savedHash,bytes:savedBytes.length,unicode_name_equal:true}),{mode:0o600});`);
 if(scenario==='settings' && input.conflict){
  source=source.replace('const response=await route.fetch();if(bodies.length===1)',`if(bodies.length===1){const competing={...bodies[0],approval_reference:'P425-CONCURRENT-APPROVED'};const concurrent=await context.request.put(route.request().url(),{headers:await route.request().allHeaders(),data:competing});assert.equal(concurrent.status(),200);}const response=await route.fetch();if(bodies.length===1)`);
 }
 if(scenario==='context'){
  source=source.replace("await context.request.get(input.baseURL+'/__p425/read-arm');", "await runtimeIdle();"+`await page.route('**/api/v1/files/*/content',async route=>{if(route.request().method()==='GET'){await route.continue({headers:{...await route.request().allHeaders(),'X-P426-Transport-Fault':'slow-read'}});}else await route.continue();});`);
  source=source.replace('await waitPageFlag(page,"/__p425/read-state","reached");',"await page.waitForTimeout(100);await page.unroute('**/api/v1/files/*/content');");
  source=source.replace("stage='Blob release';await login();","stage='Blob release';await login();await runtimeIdle();");
  source=source.replace("if(await page.locator('[data-file-save]').count())break;", "if(await page.locator('[data-file-save]').count())break;await page.waitForTimeout(200);");
 }
 if(scenario==='context')source=source.replace(".catch(()=>{console.error('real context browser failed at '+stage);process.exitCode=1;});",".catch(e=>{console.error(JSON.stringify({stage,kind:e.name,code:e.code,line:(e.stack||'').split('\\n').find(s=>s.includes('browser-scenario.cjs:'))?.trim()}));process.exitCode=1;});");
 if(scenario==='faults'){
  // Fault headers reach the forwarding proxy; it first receives the actual
  // official 200 and then corrupts transport. No synthetic success is supplied.
  source=source.replace("for(let i=0;i<4;i++){",`for(let i=0;i<4;i++){const fault=['cut','length','redirect','oversize'][i];await page.route('**/api/v1/files/*/content',async route=>route.continue({headers:{...await route.request().allHeaders(),'X-P426-Transport-Fault':fault}}));`);
  source=source.replace("const observed=await context.request.get(input.baseURL+'/__p425/download-fault-state');const count=(await observed.json()).count;if(count===i+1)break;assert.equal(count,i);assert(statuses.length>before && [409,503].includes(statuses.at(-1)));", "let state;for(let check=0;check<50;check++){state=JSON.parse(await fs.readFile(input.observationFile,'utf8'));if(state.faults[fault]===1)break;await page.waitForTimeout(20);}if(state.faults[fault]===1)break;assert.equal(state.faults[fault],0);assert(statuses.length>before && [409,503].includes(statuses.at(-1)));await page.waitForTimeout(200);");
  source=source.replace("const state=await context.request.get(input.baseURL+'/__p425/download-fault-state');assert.equal((await state.json()).count,i+1);", "assert.equal(JSON.parse(await fs.readFile(input.observationFile,'utf8')).faults[fault],1);await page.unroute('**/api/v1/files/*/content');");
 }
 const script=path.join(input.evidenceDir,'browser-scenario.cjs'),config=path.join(input.evidenceDir,'browser-input.json');
 await fs.writeFile(script,source,{mode:0o600,flag:'wx'});await fs.writeFile(config,JSON.stringify(input),{mode:0o600,flag:'wx'});
 const child=spawn(process.execPath,[script],{env:{...process.env,WEB_FILE_INPUT:config},stdio:['ignore','pipe','pipe']});
 child.stdout.pipe(process.stdout);child.stderr.pipe(process.stderr);
 process.on('SIGINT',()=>child.kill('SIGINT'));
 child.on('exit',(code,signal)=>{process.exitCode=code??(signal?130:1);});
})().catch(()=>{console.error('official browser fixture setup failed');process.exitCode=1;});
