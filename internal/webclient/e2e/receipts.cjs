"use strict";
const assert=require("node:assert/strict"),fs=require("node:fs"),vm=require("node:vm"),path=require("node:path");
let now=0,id=0,timers=new Map();const clock={setTimeout:(f,d)=>{timers.set(++id,{f,t:now+d});return id;},clearTimeout:i=>timers.delete(i)};
async function tick(ms){const end=now+ms;while(true){let next=[...timers].filter(([,v])=>v.t<=end).sort((a,b)=>a[1].t-b[1].t)[0];if(!next)break;now=next[1].t;timers.delete(next[0]);next[1].f();for(let i=0;i<8;i++)await Promise.resolve();}now=end;}
class Observer{static latest;constructor(f){this.f=f;Observer.latest=this;}observe(e){this.e=e;}disconnect(){}unobserve(){}emit(e,ratio){this.f([{target:e,intersectionRatio:ratio,isIntersecting:ratio>0}]);}}
const doc={visibilityState:"visible",hasFocus:()=>focused,addEventListener(){},removeEventListener(){}},win={addEventListener(){},removeEventListener(){}};let focused=true;
class Mutation{static latest;constructor(f){this.f=f;Mutation.latest=this;}observe(){}disconnect(){}emit(){this.f([{attributeName:"open"}]);}}
doc.body={};
const scope={window:win,document:doc,MutationObserver:Mutation,IntersectionObserver:Observer,AbortController,Error,Date,BigInt,...clock};vm.createContext(scope);vm.runInContext(fs.readFileSync(process.env.FILE_UNIT_SOURCE || path.join(__dirname,"../assets/receipts.js"),"utf8"),scope);
(async()=>{
 let context={accountKey:"A",identityKey:"A:m",conversation:"chat",conversationEpoch:1,userID:"me",kind:"direct"},calls=[],commits=0,hold=null;
 const base=()=>({conversation_id:"chat",messages:[{seq:"1",sender_user_id:"peer",text:"hi",redacted:false}],next_after_seq:"1",has_more:false,sync_proof:"A".repeat(43),proof_expires_at:new Date(Date.now()+300000).toISOString(),server_time:new Date().toISOString(),receipt_revision:"0"});
 const r=new win.Receipts({apiCall:async(p,o)=>{calls.push({p,o});if(hold)await hold;return {scanned_seq:"1",delivered_seq:"1",read_seq:p.endsWith("/read")?"1":"0",receipt_revision:"1",duplicate:false};},identityContext:()=>context,commitMessagePage:()=>{commits++;},validateMessage:()=>{},isConversationActive:()=>true,showReceiptState:()=>{}});
 await assert.rejects(r.acceptPage({...base(),messages:[{seq:"2"}]},context));assert.equal(commits,0);assert.equal(calls.length,0);
 await assert.rejects(r.acceptPage({...base(),next_after_seq:"01"},context));assert.equal(calls.length,0);
 doc.visibilityState="hidden";await r.acceptPage(base(),context);assert.equal(commits,1);assert.equal(calls.length,1);assert.equal(calls[0].o.method,"PUT");r.confirmVisible(["1"],context);await tick(1000);assert.equal(calls.length,1);
 doc.visibilityState="visible";const el={dataset:{receiptSeq:"1"},isConnected:true};r.observe(el,"1",context);Observer.latest.emit(el,.49);await tick(1000);assert.equal(calls.length,1);Observer.latest.emit(el,.5);await tick(499);assert.equal(calls.length,1);Observer.latest.emit(el,.75);await tick(1);await tick(299);assert.equal(calls.length,1);await tick(1);assert.equal(calls.length,2);assert.equal(calls[1].o.body,'{"seqs":["1"]}');
 r.contextChanged();let release;hold=new Promise(x=>release=x);const pending=r.acceptPage(base(),context);for(let i=0;i<5;i++)await Promise.resolve();context={...context,accountKey:"B",identityKey:"B:m"};r.contextChanged();release();await assert.rejects(pending,e=>e.stale===true);assert.equal(r.afterSeq,"0");hold=null;
 const big="9007199254740993";context={...context,conversationEpoch:2};r.contextChanged();await assert.rejects(r.acceptPage({...base(),messages:[{seq:9007199254740993}]},context));assert.equal(win.Receipts.parseSeq(big).toString(),big);
 r.dispose();
 // One full page becomes one ascending read batch, never an arbitrary preview sequence.
 let batchCalls=[],active=true;const batch=new win.Receipts({apiCall:async(p,o)=>{batchCalls.push({p,o});return {scanned_seq:"500",delivered_seq:"500",read_seq:p.endsWith("/read")?"500":"0",receipt_revision:"1",duplicate:false};},identityContext:()=>context,commitMessagePage:()=>{},validateMessage:()=>{},isConversationActive:()=>active,showReceiptState:()=>{}});
 await batch.acceptPage({...base(),messages:Array.from({length:500},(_,i)=>({seq:String(i+1),sender_user_id:"peer",redacted:false})),next_after_seq:"500"},context);
 active=false;for(let i=0;i<500;i++){const el={isConnected:true};batch.observe(el,String(i+1),context);Observer.latest.emit(el,.5);}await tick(1000);assert.equal(batchCalls.length,1);
 active=true;const observer=Observer.latest;for(const seq of ["501","9007199254740993"])batch.confirmVisible([seq],context);await tick(1000);assert.equal(batchCalls.length,1);
 // Re-emit the geometric visibility after activating the conversation.
 for(let i=0;i<500;i++){const el={isConnected:true};batch.observe(el,String(i+1),context);observer.emit(el,.5);}await tick(800);assert.equal(batchCalls.length,2);const seqs=JSON.parse(batchCalls[1].o.body).seqs;assert.equal(seqs.length,500);assert.equal(seqs[0],"1");assert.equal(seqs[499],"500");batch.dispose();
 // Dialog visibility interrupts the continuous exposure, without geometry/focus events.
 let modalActive=true,modalCalls=[];const modal=new win.Receipts({apiCall:async(p,o)=>{modalCalls.push(p);return {scanned_seq:"1",delivered_seq:"1",read_seq:"1",receipt_revision:"1",duplicate:false};},identityContext:()=>context,commitMessagePage:()=>{},validateMessage:()=>{},isConversationActive:()=>modalActive,showReceiptState:()=>{}});
 await modal.acceptPage(base(),context);const modalElement={isConnected:true};modal.observe(modalElement,"1",context);Observer.latest.emit(modalElement,.5);await tick(200);modalActive=false;Mutation.latest?.emit();await tick(200);modalActive=true;Mutation.latest?.emit();await tick(400);assert.equal(modalCalls.length,1,"dialog interrupted continuous500ms exposure");await tick(400);assert.equal(modalCalls.length,2,"closing dialog restarts exposure");modal.dispose();
 console.log("page validation, background delivery, 50 percent / 500 ms / 300 ms, canonical int64, and late responses PASS");
})().catch(e=>{console.error(e);process.exitCode=1;});
