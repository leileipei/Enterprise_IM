'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const pendingTimers=new Map();let timerID=0,options,fetchImpl;const window={};
const context={window,Headers,Blob,Uint8Array,TextEncoder,TextDecoder,AbortController,DOMException,URL,Date,fetch:(...a)=>fetchImpl(...a),setTimeout:(f,ms)=>{const id=++timerID;pendingTimers.set(id,{f,ms});return id},clearTimeout:id=>pendingTimers.delete(id)};
vm.runInNewContext(fs.readFileSync(process.env.FILE_UNIT_SOURCE,'utf8'),context);
assert.equal(typeof window.FileTransport,'function');
const fid='11111111-1111-4111-8111-111111111111';
let snap={identityKey:'identity',membershipId:'22222222-2222-4222-8222-222222222222',identityEpoch:1,conversation:fid,conversationEpoch:1,kind:'direct',token:'private-test-token',tokenExpiresAt:Date.now()+90000};
let errors=[];const transport=new window.FileTransport({snapshot:()=>({...snap}),onHTTPError:(status,code)=>errors.push([status,code])});
function response(chunks,[length='3',extra={}]=[]){let i=0;return {status:200,ok:true,redirected:false,headers:new Headers({'Content-Type':'application/octet-stream','Content-Length':length,'Content-Disposition':"attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A.txt",'Cache-Control':'no-store','X-Content-Type-Options':'nosniff',...extra}),body:{getReader(){return {read:async()=>i<chunks.length?{value:new Uint8Array(chunks[i++]),done:false}:{done:true},cancel:async()=>{},releaseLock(){}}}}}}
(async()=>{
 fetchImpl=async(path,o)=>{assert.equal(path,`/api/v1/files/${fid}/content`);options=o;return response([[1,2],[3]])};
 let got=await transport.readDownload(fid,new AbortController().signal);assert.equal(got.filename,'报告.txt');assert.deepEqual([...new Uint8Array(await got.blob.arrayBuffer())],[1,2,3]);
 assert.equal(options.credentials,'omit');assert.equal(options.cache,'no-store');assert.equal(options.redirect,'error');assert.equal(options.headers.get('Authorization'),'Bearer private-test-token');assert.equal(options.headers.get('X-Acting-Membership-ID'),snap.membershipId);assert.equal(pendingTimers.size,0);
 for(const [chunks,length,extra] of [ [[[1,2]],'3',{}],[[[1,2,3,4]],'3',{}],[[[1,2,3]],'3',{'Content-Encoding':'gzip'}],[[[1,2,3]],'3',{'Content-Type':'application/json'}],[[[1,2,3]],'3',{'Location':'https://other.test'}],[[[1,2,3]],'26214401',{}],[[[1]],'01',{}]]){fetchImpl=async()=>response(chunks,[length,extra]);await assert.rejects(transport.readDownload(fid,new AbortController().signal));assert.equal(pendingTimers.size,0)}
 fetchImpl=async()=>{let r=response([[1,2]]);const old=r.body.getReader;r.body.getReader=()=>{let reader=old();reader.cancel=()=>new Promise(()=>{});return reader};return r};
 await assert.rejects(Promise.race([transport.readDownload(fid,new AbortController().signal),new Promise(resolve=>setTimeout(()=>resolve('hung cancellation'),150))]),/下载未完整/);
 fetchImpl=async()=>{snap.identityEpoch++;return response([[1,2,3]])};await assert.rejects(transport.readDownload(fid,new AbortController().signal));
 fetchImpl=async()=>{let r=response([[1,2,3]]);const old=r.body.getReader;r.body.getReader=()=>{let reader=old();const read=reader.read;reader.read=async()=>{const v=await read();snap.conversationEpoch++;return v};return reader};return r};await assert.rejects(transport.readDownload(fid,new AbortController().signal));
 fetchImpl=async(_,o)=>new Promise((resolve,reject)=>o.signal.addEventListener('abort',()=>reject(new DOMException('aborted','AbortError')),{once:true}));
 const inFlight=transport.readDownload(fid,new AbortController().signal);await Promise.resolve();assert([...pendingTimers.values()].some(t=>t.ms===65000));transport.contextChanged();await assert.rejects(inFlight);assert.equal(pendingTimers.size,0);
 const expired=transport.readDownload(fid,new AbortController().signal);await Promise.resolve();let expiry=[...pendingTimers.values()].find(t=>t.ms!==65000);assert(expiry);expiry.f();await assert.rejects(expired);assert.equal(pendingTimers.size,0);
 fetchImpl=async()=>({status:403,ok:false,json:async()=>({error_code:'invalid_identity',content:'never disclose this body'})});await assert.rejects(transport.readDownload(fid,new AbortController().signal),e=>!String(e).includes('never disclose'));assert.deepEqual(errors.at(-1),[403,'invalid_identity']);
 const file=new Blob([new Uint8Array([1,2,3])]);fetchImpl=async(_,o)=>{assert.equal(o.method,'PUT');assert.equal(o.body,file);assert.equal(o.headers.get('Content-Type'),'application/octet-stream');return {ok:true,status:200,json:async()=>({file_id:fid,state:'uploaded'})}};const uploaded=await transport.putFile(fid,file,new AbortController().signal);assert.equal(uploaded.state,'uploaded');assert.equal(pendingTimers.size,0);
 console.log('fileTransportHeaders, fileTransportTruncatedBody, fileTransportIdentityEpoch: PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
