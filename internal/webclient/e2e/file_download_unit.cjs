'use strict';const assert=require('node:assert/strict'),{environment}=require('./file_unit_helpers.cjs');
const env=environment('file-download'),{window,document,context,tick}=env;
const id='22222222-2222-4222-8222-222222222222';let ctx={identityKey:'actor',membershipId:id,identityEpoch:1,conversation:id,conversationEpoch:1,kind:'direct'},enabled=true,broken=false,held=null,created=[],revoked=[];
context.URL={createObjectURL:b=>{assert.equal(b.size,3);const u='blob:unit-'+created.length;created.push(u);return u},revokeObjectURL:u=>revoked.push(u)};
const container=document.getElementById('save');const transport={readDownload:async()=>{if(broken)throw new Error('truncated');if(held)return new Promise(resolve=>held=resolve);return {filename:'中文报告.txt',blob:new Blob(['abc'])}}};
const flow=new window.FileDownload({transport,context:()=>({...ctx}),capabilities:()=>({download_enabled:enabled}),saveContainer:container});
(async()=>{
 const ft=window.FileTransport;assert.equal(ft.dispositionFilename("attachment; filename*=UTF-8''%E4%B8%AD%E6%96%87.txt"),'中文.txt');
 for(const s of ['attachment; filename="../x"','attachment; filename="x\r\n"',"attachment; filename*=UTF-8''%ZZ",'attachment; filename=x; filename=y'])assert.throws(()=>ft.dispositionFilename(s));
 enabled=false;await assert.rejects(flow.request(id));enabled=true;broken=true;await assert.rejects(flow.request(id));assert.equal(created.length,0);assert.equal(container.querySelectorAll('a').length,0);broken=false;
 await flow.request(id);assert.equal(created.length,1);assert.equal(container.querySelectorAll('a').length,1);assert.equal(container.querySelectorAll('a')[0].download,'中文报告.txt');assert(!revoked.length);await assert.rejects(flow.request(id));flow.save();assert.deepEqual(revoked,created);assert.equal(container.querySelectorAll('a').length,0);
 await flow.request(id);await tick(59999);assert.equal(revoked.length,1);await tick(1);assert.deepEqual(revoked,created);assert.equal(container.querySelectorAll('a').length,0);
 await flow.request(id);ctx.conversationEpoch++;flow.contextChanged();assert.deepEqual(revoked,created);
 held=true;const pending=flow.request(id);await env.flush();await assert.rejects(flow.request(id));ctx.identityEpoch++;flow.contextChanged();held({filename:'late.txt',blob:new Blob(['abc'])});held=null;await assert.rejects(pending);assert.equal(created.length,3);assert.equal(container.querySelectorAll('a').length,0);assert.equal(env.timers.size,0);
 console.log('fileDownloadDispositionSafety, fileDownloadNoPartialSave, fileDownloadBlobLifetime, fileDownloadOneAtTime: PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
