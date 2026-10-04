'use strict';const assert=require('node:assert/strict'),{environment}=require('./file_unit_helpers.cjs');
const env=environment('file-transfer'),{window,document,timers,tick,flush}=env;
const conv='11111111-1111-4111-8111-111111111111',fid='22222222-2222-4222-8222-222222222222';
let ctx={identityKey:'actor',membershipId:fid,identityEpoch:1,conversation:conv,conversationEpoch:1,kind:'direct'},ready=[],clears=0,reserves=[],puts=[],queries=0,reserveFails=false,putFails=false,state='uploaded',holdQuery=null,maxActive=0,active=0;
const policy={enabled:true,max_size_bytes:'26214400',allowed_media_types:['application/pdf','image/png','image/jpeg','text/plain'],upload_ttl_seconds:'300'};
const status=()=>({file_id:fid,conversation_id:conv,original_filename:'报告.txt',declared_media_type:'text/plain',declared_size_bytes:'3',state,state_version:'1',created_at:new Date(env.now()).toISOString(),upload_expires_at:new Date(env.now()+300000).toISOString()});
const request=async(path,options={})=>{if(path==='/api/v1/file-upload-policy')return policy;if(options.method==='POST'){reserves.push(JSON.parse(options.body));if(reserveFails){reserveFails=false;throw new Error('unknown reservation')}return {...status(),state:'allocated',state_version:'0'}}queries++;active++;maxActive=Math.max(maxActive,active);try{return holdQuery?await new Promise(resolve=>holdQuery=resolve):status()}finally{active--}};
const transport={putFile:async(id,file)=>{puts.push({id,file});if(putFails){putFails=false;throw new Error('unknown PUT')}return status()}};
const flow=new window.FileTransfer({request,transport,context:()=>({...ctx}),onReady:r=>ready.push(r),onClear:()=>clears++});
const file={name:'报告.txt',type:'text/plain',size:3};
(async()=>{
 for(const bad of [{...file,name:'字'.repeat(86)},{...file,size:26214401},{...file,name:'../x.txt'},{...file,size:0}])await assert.rejects(flow.select(bad));assert.equal(reserves.length,0);
 await flow.select(file);reserveFails=true;await assert.rejects(flow.upload());assert.equal(puts.length,0);await flow.retryOriginal();assert.deepEqual(reserves[0],reserves[1]);assert.equal(puts.length,1);
 assert([...timers.values()].some(t=>t.ms===2000));assert(![...timers.values()].some(t=>t.ms===10000));
 document.hidden=true;document.fire('visibilitychange');const before=queries;await tick(6000);assert.equal(queries,before);document.hidden=false;document.fire('visibilitychange');await tick(2000);assert(queries>before);
 await tick(120000);const end=queries;await tick(5000);assert.equal(queries,end);assert.equal(maxActive,1);
 flow.contextChanged();assert.equal(timers.size,0);await flow.select(file);putFails=true;await assert.rejects(flow.upload());assert.equal(puts.length,2);state='allocated';await flow.queryStatus();assert.equal(puts.length,2);state='uploaded';await flow.queryStatus();assert.equal(puts.length,2);
 state='ready';await flow.queryStatus();assert.equal(ready.length,1);assert.equal(ready[0].fileID,fid);assert.equal(timers.size,0);await assert.rejects(flow.select(file), /请先取消/);
 for(const rejected of ['rejected','scan_failed','delete_pending','deleted']){state=rejected;await flow.queryStatus();assert.equal(ready.length,1)}
 state='scanning';holdQuery=true;let late=flow.queryStatus();await flush();ctx.conversationEpoch++;flow.contextChanged();holdQuery({...status(),state:'ready'});holdQuery=null;try{await late}catch(_){}assert.equal(ready.length,1);assert.equal(timers.size,0);
 assert(clears>0);console.log('fileTransferReserveReplay, fileTransferUnknownPut, fileTransferPollWindow, fileTransferRejectScan, fileTransferHiddenPage: PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
