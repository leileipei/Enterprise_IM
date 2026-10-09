"use strict";
const assert=require("node:assert/strict"),fs=require("node:fs"),vm=require("node:vm"),path=require("node:path");
const scope={window:{},AbortController,Date,Error,crypto:require("node:crypto").webcrypto};vm.createContext(scope);vm.runInContext(fs.readFileSync(process.env.FILE_UNIT_SOURCE || path.join(__dirname,"../assets/device-session.js"),"utf8"),scope);
(async()=>{
 let context={accountKey:"A",identityKey:"A:m",membershipId:"m"},calls=[],quota=false,hold=null;const id="00000000-0000-4000-8000-000000000001",token="A".repeat(43);
 const apiCall=async(p,o,c)=>{calls.push({p,o,c});if(o?.method==="POST"){if(quota){const e=new Error("quota");e.code="device_quota";e.status=409;throw e;}if(hold)await hold;return {device_id:id,device_token:token,expires_at:new Date(Date.now()+86400000).toISOString(),server_time:new Date().toISOString()};}return {devices:[],has_more:false};};
 const make=()=>new scope.window.DeviceSession({apiCall,identityContext:()=>context,uuidV7:()=>"00000000-0000-7000-8000-000000000001",enabled:()=>true});
 const d=make();const cred=await d.ensure(context);assert.equal(cred.token,token);await d.ensure(context);assert.equal(calls.filter(c=>c.o?.method==="POST").length,1);
 context={...context,identityKey:"A:n",membershipId:"n"};await d.ensure(context);assert.equal(calls.filter(c=>c.o?.method==="POST").length,1);
 await d.revoke(id);assert.equal(d.credential(context),null);await d.ensure(context);await d.dispose();assert.equal(d.credential(context),null);
 const disabled=new scope.window.DeviceSession({apiCall,identityContext:()=>context,enabled:()=>false});const before=calls.length;assert.equal(await disabled.ensure(context),null);assert.equal(calls.length,before);
 quota=true;const q=make();await assert.rejects(q.ensure(context));assert.equal(q.credential(context),null);quota=false;await q.revoke(id);await q.ensure(context);
 const refreshed=make();await refreshed.ensure(context);assert.equal(calls.filter(c=>c.o?.method==="POST").length,5);
 let release;hold=new Promise(r=>release=r);const late=make(),pending=late.ensure(context);await Promise.resolve();context={accountKey:"B",identityKey:"B:m",membershipId:"m"};release();await assert.rejects(pending,e=>e.stale===true);assert.equal(late.credential(context),null);
 let recoveryCalls=[],attempt=0,reg=0;context={accountKey:"C",identityKey:"C:m",membershipId:"m"};
 const recovery=new scope.window.DeviceSession({apiCall:async(p,o)=>{recoveryCalls.push({p,o});if(o.method==="POST"){attempt++;if(attempt===1)throw new Error("network result lost");if(attempt===2){const e=new Error("duplicate");e.code="registration_exists";e.deviceID=id;throw e;}return {device_id:id,device_token:token,expires_at:new Date(Date.now()+86400000).toISOString(),server_time:new Date().toISOString()};}return null;},identityContext:()=>context,uuidV7:()=>"00000000-0000-7000-8000-"+String(++reg).padStart(12,"0"),enabled:()=>true});
 await assert.rejects(recovery.ensure(context));await recovery.ensure(context);assert.equal(recoveryCalls.filter(c=>c.o.method==="DELETE").length,1);const posts=recoveryCalls.filter(c=>c.o.method==="POST").map(c=>JSON.parse(c.o.body).registration_id);assert.equal(posts[0],posts[1]);assert.notEqual(posts[1],posts[2]);
 assert.equal(scope.localStorage,undefined);assert.equal(scope.sessionStorage,undefined);console.log("device memory, quota recovery, revoke, disabled, refresh and late account response PASS");
})().catch(e=>{console.error(e);process.exitCode=1;});
