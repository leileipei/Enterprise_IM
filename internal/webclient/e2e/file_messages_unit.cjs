'use strict';const assert=require('node:assert/strict'),{environment}=require('./file_unit_helpers.cjs');
const env=environment('file-messages'),{window}=env;
const conv='11111111-1111-4111-8111-111111111111',fid='22222222-2222-4222-8222-222222222222',cid='019a0000-0000-7000-8000-000000000001';
let ctx={identityKey:'actor',membershipId:fid,identityEpoch:1,conversation:conv,conversationEpoch:1,kind:'direct'},allowed=true,pending=[],acks=[],requests=[],fail=true,late=null;
const ack=()=>({message_id:fid,conversation_id:conv,seq:1,server_time:new Date().toISOString(),duplicate:false});
const flow=new window.FileMessages({request:async(p,o)=>{requests.push({path:p,body:JSON.parse(o.body)});if(late)return new Promise(resolve=>late=resolve);if(fail)throw new Error('unknown');return ack()},context:()=>({...ctx}),uuidV7:()=>cid,canSend:()=>allowed,onPending:v=>pending.push(v),onACK:a=>acks.push(a)});
const ready=()=>({fileID:fid,context:{...ctx}});
(async()=>{
 allowed=false;assert.throws(()=>flow.attach(ready()));allowed=true;flow.attach(ready());
 for(const bad of ['x'.repeat(16385),'x\0','\ud800'])await assert.rejects(flow.send(bad));assert.equal(requests.length,0);
 await assert.rejects(flow.send('中'.repeat(5461)+'x'));await assert.rejects(flow.retry());assert.deepEqual(requests[0],requests[1]);assert.equal(acks.length,0);assert.equal(requests[0].body.caption.length,5462);assert.equal(requests[0].body.client_msg_id,cid);assert(pending.includes(true));assert.throws(()=>flow.attach(ready()));
 fail=false;await flow.retry();assert.equal(acks.length,1);assert.equal(pending.at(-1),false);
 const privateName='<img src=x onerror=alert(1)>报告.txt';const file={seq:2,message_type:'file',caption:'caption',attachment:{file_id:fid,available:true,download_available:false,original_filename:privateName,actual_size_bytes:'3',detected_media_type:'text/plain'}};
 const card=flow.render(file);assert(card.textContent.includes(privateName));assert(!card.textContent.includes('SHA'));assert.equal(card.querySelectorAll('img').length,0);
 for(const redacted of [{...file,redacted:true},{...file,attachment:{...file.attachment,available:false}}]){const c=flow.render(redacted);assert(!c.textContent.includes(privateName));if(redacted.redacted)assert(!c.textContent.includes('caption'))}
 assert.equal(flow.render({seq:3,text:'附件消息（当前客户端不支持查看）'}).textContent,'附件消息（当前客户端不支持查看）');
 ctx.kind='group';flow.attach(ready());late=true;const waiting=flow.send('late');await env.flush();ctx.identityEpoch++;flow.contextChanged();late(ack());try{await waiting}catch(_){}assert.equal(acks.length,1);assert.equal(pending.at(-1),false);assert(requests.at(-1).path.includes('/groups/'));
 console.log('fileMessageUnknownRetry, fileMessageSharedPendingSlot, fileMessageTypedRedaction, fileMessageLegacyFallback: PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
