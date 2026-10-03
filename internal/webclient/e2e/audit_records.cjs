"use strict";
const assert=require("node:assert/strict"),http=require("node:http"),fs=require("node:fs"),path=require("node:path");
const {chromium}=require("playwright");
const assets=path.join(__dirname,"..","assets");
const tenant="00000000-0000-4000-8000-000000000001",user="00000000-0000-4000-8000-000000000002",admin="00000000-0000-4000-8000-000000000003",employee="00000000-0000-4000-8000-000000000004";
const otherUser="abcdefab-cdef-4abc-8abc-abcdefabcdef";
const rows=Array.from({length:23},(_,i)=>({id:(9007199254740995n-BigInt(i)).toString(),actor_user_id:user,acting_membership_id:admin,action:"retention_policy_update",resource_type:"tenant",resource_id:i===22?null:tenant,outcome:i%2?"deny":"allow",reason:i===21?"<img src=x onerror=alert(1)>version_conflict":i%2?"version_conflict":"approved_retention_change",occurred_at:"2026-10-03T10:00:00.123456000Z"}));
// Newer fractional timestamp outranks ID, even below millisecond resolution.
rows[0].id="9";rows[0].occurred_at="2026-10-03T10:00:00.123456002Z";rows[1].id="10";rows[1].occurred_at="2026-10-03T10:00:00.123456001Z";
const otherRows=Array.from({length:21},(_,i)=>({...rows[2],id:String(7000-i),actor_user_id:otherUser,reason:"other_actor",occurred_at:"2026-10-02T10:00:00Z"}));
const timeRows=Array.from({length:25},(_,i)=>({...rows[2],id:String(6000-i),occurred_at:`2026-10-03T10:00:00.${String(25-i).padStart(6,"0")}Z`}));
const fixtureTime=value=>BigInt(Date.parse(value.replace(/\.\d+/,"")))*1000000n+BigInt((/\.(\d+)/.exec(value)?.[1]||"").padEnd(9,"0"));
const resourceTarget="dddddddd-dddd-4ddd-8ddd-dddddddddddd",resourceOther="eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee";
const resourceRows=Array.from({length:24},(_,i)=>({...rows[2],id:String(5000-i),resource_type:i===23?"tenant":"conversation",resource_id:i===21?null:i===22?resourceOther:resourceTarget,outcome:i%2?"deny":"allow"}));
let resourceDataset=false,timeDataset=false;
let port,mode="",held=[],writes=0,empty=false,cycle=false;
const calls=[];
const server=http.createServer((req,res)=>{
 const url=new URL(req.url,`http://127.0.0.1:${port}`),send=(code,body,type="application/json")=>{res.writeHead(code,{"Content-Type":type,"Cache-Control":"no-store"});res.end(typeof body==="string"?body:JSON.stringify(body));};
 if(["/web/","/web/app.js","/web/retention.js","/web/legal-holds.js","/web/retention-policy.js","/web/retention-history.js","/web/audit.js", "/web/message-search.js", "/web/cross-message-search.js","/web/style.css"].includes(url.pathname)){const name=url.pathname==="/web/"?"index.html":url.pathname.slice(5);if(!fs.existsSync(path.join(assets,name)))return send(404,{});return send(200,fs.readFileSync(path.join(assets,name),"utf8"),name.endsWith(".js")?"text/javascript":name.endsWith(".css")?"text/css":"text/html");}
 if(url.pathname==="/web/config")return send(200,{issuer:"https://sso.example.test/group",authorization_url:`http://127.0.0.1:${port}/authorize`,client_id:"enterprise-im-web",redirect_url:`http://127.0.0.1:${port}/web/`,scope:"openid profile"});
 if(url.pathname==="/authorize"){res.writeHead(302,{Location:`/web/?code=fixture&state=${url.searchParams.get("state")}`});return res.end();}
 if(url.pathname==="/web/oauth/token")return send(200,{access_token:"fixture-token",token_type:"Bearer",expires_in:300});
 if(req.headers.authorization!=="Bearer fixture-token")return send(401,{error_code:"unauthorized"});
 if(url.pathname==="/api/v1/me")return send(200,{tenant_id:tenant,user_id:user,display_name:"管理员",global_employee_no:"A001",memberships:[{id:admin,organization_name:"集团总部",legal_entity_name:"总部法人",title:"管理员",is_primary:true},{id:employee,organization_name:"分公司",legal_entity_name:"分公司法人",title:"员工",is_primary:false}]});
 const actor=req.headers["x-acting-membership-id"];
 if(url.pathname.startsWith("/api/v1/admin/")&&actor!==admin)return send(404,{error_code:"not_found"});
 if(url.pathname==="/api/v1/admin/retention-policy"){if(req.method!=="GET"){writes++;return send(503,{error_code:"unavailable"});}return send(200,{message_body_days:365,version:0,approval_reference:"",approved_by_user_id:"",approved_at:null});}
 if(url.pathname==="/api/v1/admin/audit-events"){
  assert.equal(req.method,"GET");assert.equal(url.searchParams.get("limit"),"20");assert.equal(actor,admin);assert.equal([...url.searchParams.keys()].some(k=>!["limit","cursor","action","outcome","actor_user_id","from","until","resource_type","resource_id"].includes(k)),false);
  const action=url.searchParams.get("action")||"",outcome=url.searchParams.get("outcome")||"",cursor=url.searchParams.get("cursor")||"";const actorFilter=url.searchParams.get("actor_user_id")||"";const from=url.searchParams.get("from")||"",until=url.searchParams.get("until")||"";const resourceType=url.searchParams.get("resource_type")||"",resourceID=url.searchParams.get("resource_id")||"";calls.push({action,outcome,cursor,actor,actorFilter,from,until,resourceType,resourceID});const fault=mode;mode="";
  if(["400","401","403","404","503"].includes(fault))return send(+fault,{error_code:"rejected"});
  const filtered=cycle?Array.from({length:60},(_,i)=>({...rows[2],id:String(8000-i)})):empty?[]:(resourceDataset?resourceRows:timeDataset?timeRows:actorFilter?[...rows,...otherRows]:rows).filter(e=>(!action||e.action===action)&&(!outcome||e.outcome===outcome)&&(!actorFilter||e.actor_user_id===actorFilter)&&(!from||fixtureTime(e.occurred_at)>=fixtureTime(from))&&(!until||fixtureTime(e.occurred_at)<fixtureTime(until))&&(!resourceType||e.resource_type===resourceType)&&(!resourceID||e.resource_id===resourceID));
  const start=cycle?(cursor==="A"?20:cursor==="B"?40:0):cursor?20:0;let result={events:filtered.slice(start,start+20).map(e=>({...e,text:"NEVER_BODY",content_digest:"NEVER_DIGEST"})),next_cursor:cycle?(start===0?"A":start===20?"B":"A"):filtered.length>start+20?"page-20":""};
  if(fault==="duplicate")result.events[1]={...result.events[0]};
  if(fault==="order")result.events.reverse();
  if(fault==="numeric-id")result.events[0].id=9007199254740992;
  if(fault==="overflow-id")result.events[0].id="9223372036854775808";
  if(fault==="leading-zero")result.events[0].id="09";
  if(fault==="bad-date")result.events[0].occurred_at="2026-02-30T10:00:00Z";
  if(fault==="resource")result.events[0].resource_id="invalid";
  if(fault==="outcome")result.events[0].outcome="accepted";
  if(fault==="resource-type-escape")result.events[0].resource_type="tenant";
  if(fault==="resource-id-escape")result.events[0].resource_id=resourceOther;
  if(fault==="resource-null-escape")result.events[0].resource_id=null;
  if(fault==="time-before")result.events.at(-1).occurred_at="2026-10-03T10:00:00.000002Z";
  if(fault==="time-end")result.events[0].occurred_at="2026-10-03T10:00:00.000024Z";
  if(fault==="actor-escape")result.events[0].actor_user_id=user;
  if(fault==="filter-escape")result.events[0].action="group_invite";
  if(fault==="cursor-loop"){result.events=Array.from({length:20},(_,i)=>({...rows[2],id:String(1000-i)}));result.next_cursor=cursor;}
  if(fault==="cross-page-order")result.events[0]={...rows[0],id:"9223372036854775807"};
  if(fault==="big-int-order"){[result.events[2],result.events[3]]=[result.events[3],result.events[2]];}
  if(fault==="nanosecond-order"){[result.events[0],result.events[1]]=[result.events[1],result.events[0]];}
  if(fault==="hold"||fault==="timeout"){held.push(()=>send(200,result));return;}
  return send(200,result);
 }
 if(url.pathname==="/api/v1/conversations")return send(200,{conversations:[],has_more:false});
 if(url.pathname==="/api/v1/groups")return send(200,{groups:[],has_more:false});
 return send(404,{error_code:"not_found"});
});
server.listen(0,"127.0.0.1",async()=>{
 port=server.address().port;let browser,page;
 try{
  browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE||undefined});page=await browser.newPage({viewport:{width:1280,height:850}});page.setDefaultTimeout(3500);const errors=[];page.on("pageerror",e=>errors.push(e.message));
  await page.goto(`http://127.0.0.1:${port}/web/`);await page.locator("#login-button").click();await page.locator("#workspace").waitFor({state:"visible"});
  const actor=async name=>page.locator("#identity-options").getByRole("button",{name:new RegExp(name)}).click();await actor("集团总部");
  const entry=page.locator("#audit-open");assert.equal(await entry.count(),1,"audit query entry must exist");await entry.click();
  const actorFilter=page.locator("#audit-actor");assert.equal(await actorFilter.count(),1,"actor filter input must exist");
  const from=page.locator("#audit-from"),until=page.locator("#audit-until");assert.equal(await from.count(),1,"time range inputs must exist");assert.equal(await until.count(),1);
  const resourceType=page.locator("#audit-resource-type"),resourceID=page.locator("#audit-resource-id");assert.equal(await resourceType.count(),1,"resource inputs must exist");assert.equal(await resourceID.count(),1);
  const cards=page.locator(".audit-record-card"),hint=page.locator("#audit-hint"),more=page.locator("#audit-more"),refresh=page.locator("#audit-refresh"),apply=page.locator("#audit-apply"),action=page.locator("#audit-action"),outcome=page.locator("#audit-outcome"),close=page.locator("#audit-close");
  await hint.getByText(/已显示 20/).waitFor();assert.equal(await cards.count(),20);assert.match(await cards.first().innerText(),/事件 9/);assert.match(await cards.nth(2).innerText(),/9007199254740993/);
  await more.click();await hint.getByText(/已显示 23/).waitFor();assert.equal(await cards.count(),23);assert.equal(await more.isVisible(),false);assert.match(await cards.last().innerText(),/未指定/);assert.equal(await cards.locator("img").count(),0);assert.equal((await cards.allTextContents()).join("").includes("NEVER_"),false);assert.equal(writes,0);
  if(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR){fs.mkdirSync(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,{recursive:true});await page.locator("#audit-dialog").evaluate(d=>{d.scrollTop=0;});await page.screenshot({path:path.join(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,"desktop.png")});await apply.scrollIntoViewIfNeeded();await page.screenshot({path:path.join(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,"desktop-form-bottom.png")});await page.setViewportSize({width:390,height:844});await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));await page.locator("#audit-dialog").evaluate(d=>{d.scrollTop=0;});await page.screenshot({path:path.join(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,"mobile-form.png")});await apply.scrollIntoViewIfNeeded();await page.screenshot({path:path.join(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,"mobile-form-bottom.png")});await cards.last().scrollIntoViewIfNeeded();await page.screenshot({path:path.join(process.env.IM_TEST_AUDIT_SCREENSHOT_DIR,"mobile.png")});assert.equal(await page.locator("#audit-dialog").evaluate(d=>d.scrollWidth<=d.clientWidth+1),true);await page.setViewportSize({width:1280,height:850});}
  await actorFilter.fill(otherUser.toUpperCase());assert.equal(await cards.count(),0);await apply.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).actorFilter,otherUser);assert.equal(calls.at(-1).cursor,"");assert.equal((await cards.allTextContents()).every(s=>s.includes(otherUser)),true);
  await more.click();await hint.getByText(/已显示 21/).waitFor();assert.equal(await cards.count(),21);assert.equal(calls.at(-1).actorFilter,otherUser);
  let actorCalls=calls.length;await actorFilter.fill("invalid UUID");await apply.click();await hint.getByText(/执行人.*UUID/).waitFor();assert.equal(calls.length,actorCalls);assert.equal(await cards.count(),0);
  await actorFilter.fill("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb");await apply.click();await hint.getByText(/暂无审计记录/).waitFor();
  await actorFilter.fill(otherUser);mode="actor-escape";await apply.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await actorFilter.fill(user);await apply.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).cursor,"");held.shift()();await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));assert.equal((await cards.allTextContents()).some(s=>s.includes(otherUser)),false);
  await actorFilter.fill("");await apply.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).actorFilter,"");
  timeDataset=true;await from.fill("2026-10-03T18:00:00.000003+08:00");await until.fill("2026-10-03T10:00:00.000024Z");assert.equal(await cards.count(),0);await apply.click();await hint.getByText(/已显示 20/).waitFor();assert.match(await cards.first().innerText(),/事件 5998/);assert.equal(calls.at(-1).cursor,"");assert.equal(calls.at(-1).from,"2026-10-03T18:00:00.000003+08:00");await more.click();await hint.getByText(/已显示 21/).waitFor();assert.match(await cards.last().innerText(),/事件 5978/);
  for(const bad of ["2026-02-30T10:00:00Z","2026-10-03T10:00:00","2026-10-03T10:00:00.1234567Z","0001-01-01T00:00:00+01:00"]){let n=calls.length;await from.fill(bad);await apply.click();await hint.getByText(/时间格式/).waitFor();assert.equal(calls.length,n);assert.equal(await cards.count(),0);}
  for(const bad of ["2026-10-03T10:00:00.000024Z","2026-10-03T18:00:00.000024+08:00","2026-10-03T10:00:00.000025Z"]){let n=calls.length;await from.fill(bad);await apply.click();await hint.getByText(/开始时间.*早于/).waitFor();assert.equal(calls.length,n);}
  await from.fill("2026-10-03T10:00:00.000003Z");for(const fault of ["time-before","time-end"]){mode=fault;await apply.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);}
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await until.fill("2026-10-03T10:00:00.000004Z");await apply.click();await hint.getByText(/已显示 1/).waitFor();assert.equal(calls.at(-1).cursor,"");held.shift()();await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));assert.equal(await cards.count(),1);assert.match(await cards.first().innerText(),/事件 5978/);
  await from.fill("");await apply.click();await hint.getByText(/已显示 3/).waitFor();await from.fill("2026-10-03T10:00:00.000026Z");await until.fill("");await apply.click();await hint.getByText(/暂无审计记录/).waitFor();
  await from.fill("2026-10-03T10:00:00.000003Z");await close.click();assert.equal(await from.inputValue(),"");assert.equal(await until.inputValue(),"");timeDataset=false;await entry.click();await hint.getByText(/已显示 20/).waitFor();
  resourceDataset=true;await resourceType.fill("conversation");await resourceID.fill(resourceTarget.toUpperCase());await apply.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).resourceID,resourceTarget);assert.equal(calls.at(-1).cursor,"");await more.click();await hint.getByText(/已显示 21/).waitFor();assert.equal((await cards.allTextContents()).every(s=>s.includes(resourceTarget)&&s.includes("conversation")),true);
  await actorFilter.fill(user);await from.fill("2026-10-03T10:00:00.123456Z");await until.fill("2026-10-03T10:00:00.123457Z");await outcome.selectOption("allow");await apply.click();await hint.getByText(/已显示 11/).waitFor();assert.equal((await cards.allTextContents()).every(s=>s.includes("允许")),true);await actorFilter.fill("");await from.fill("");await until.fill("");await outcome.selectOption("");
  for(const bad of ["Bad","bad type","a".repeat(65)]){let n=calls.length;await resourceType.fill(bad);await apply.click();await hint.getByText(/资源类型格式/).waitFor();assert.equal(calls.length,n);}
  await resourceType.fill("");let resourceCalls=calls.length;await apply.click();await hint.getByText(/资源 ID.*资源类型/).waitFor();assert.equal(calls.length,resourceCalls);await resourceType.fill("conversation");await resourceID.fill("bad UUID");await apply.click();await hint.getByText(/资源 ID.*UUID/).waitFor();assert.equal(calls.length,resourceCalls);
  await resourceID.fill(resourceTarget);for(const fault of ["resource-type-escape","resource-id-escape","resource-null-escape"]){mode=fault;await apply.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);}
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await resourceID.fill(resourceOther);await apply.click();await hint.getByText(/已显示 1/).waitFor();assert.equal(calls.at(-1).cursor,"");held.shift()();await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));assert.equal(await cards.count(),1);assert.match(await cards.first().innerText(),/事件 4978/);
  await resourceID.fill(resourceTarget);mode="hold";await apply.click();await hint.getByText(/正在加载/).waitFor();await resourceType.fill("tenant");await apply.click();await hint.getByText(/已显示 1/).waitFor();held.shift()();await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));assert.equal(await cards.count(),1);assert.match(await cards.first().innerText(),/事件 4977/);
  await resourceType.fill("unrecorded_type");await apply.click();await hint.getByText(/暂无审计记录/).waitFor();await resourceType.fill("conversation");await resourceID.fill("");await apply.click();await hint.getByText(/已显示 20/).waitFor();await more.click();await hint.getByText(/已显示 23/).waitFor();assert.equal((await cards.allTextContents()).some(s=>s.includes("未指定")),true);
  await close.click();assert.equal(await resourceType.inputValue(),"");assert.equal(await resourceID.inputValue(),"");resourceDataset=false;await entry.click();await hint.getByText(/已显示 20/).waitFor();
  await action.fill("retention_policy_update");assert.equal(await cards.count(),0);await outcome.selectOption("deny");await apply.click();await hint.getByText(/已显示 11/).waitFor();assert.equal(calls.at(-1).cursor,"");assert.equal(calls.at(-1).outcome,"deny");assert.equal((await cards.allTextContents()).every(s=>s.includes("拒绝")),true);
  let count=calls.length;await action.fill("Bad action");await apply.click();await hint.getByText(/动作格式/).waitFor();assert.equal(calls.length,count);assert.equal(await cards.count(),0);
  await action.fill("unrecorded_action");await apply.click();await hint.getByText(/暂无审计记录/).waitFor();await action.fill("");await outcome.selectOption("");await apply.click();await hint.getByText(/已显示 20/).waitFor();
  for(const fault of ["duplicate","order","numeric-id","overflow-id","leading-zero","bad-date","resource","outcome","nanosecond-order","big-int-order"]){mode=fault;await refresh.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);await refresh.click();await hint.getByText(/已显示 20/).waitFor();}
  for(const fault of ["503","cursor-loop","cross-page-order"]){mode=fault;await more.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);await refresh.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).cursor,"");}
  // Full valid pages and unique decreasing records isolate A -> B -> A cursor reuse.
  cycle=true;await refresh.click();await hint.getByText(/已显示 20/).waitFor();await more.click();await hint.getByText(/已显示 40/).waitFor();await more.click();
  await page.waitForFunction(()=>/加载失败|已显示 60/.test(document.getElementById("audit-hint").textContent));assert.match(await hint.innerText(),/加载失败/);assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);
  cycle=false;await refresh.click();await hint.getByText(/已显示 20/).waitFor();
  await action.fill("retention_policy_update");mode="filter-escape";await apply.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);
  // Changed filter invalidates a delayed request and starts from the first page.
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await outcome.selectOption("deny");await apply.click();await hint.getByText(/已显示 11/).waitFor();held.shift()();await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));assert.equal(await cards.count(),11);
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await close.click();held.shift()();assert.equal(await cards.count(),0);await entry.click();await hint.getByText(/已显示 23|已显示 20/).waitFor();assert.equal(await actorFilter.inputValue(),"");
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await close.click();await actor("分公司");held.shift()();assert.equal(await cards.count(),0);await page.waitForFunction(()=>document.getElementById("audit-open").classList.contains("hidden"));await actor("集团总部");await entry.click();await hint.getByText(/已显示 20/).waitFor();
  for(const status of ["403","404"]){mode=status;await more.click();await hint.getByText(/权限已失效/).waitFor();assert.equal(await cards.count(),0);assert.equal(await apply.isDisabled(),true);assert.equal(await entry.isVisible(),false);await close.click();await actor("分公司");await actor("集团总部");await entry.click();await hint.getByText(/已显示 20/).waitFor();}
  mode="timeout";await refresh.click();await hint.getByText(/加载失败/).waitFor({timeout:20000});assert.equal(await cards.count(),0);await refresh.click();await hint.getByText(/已显示 20/).waitFor();held.shift()();
  // Unknown retention write keeps its existing guard and cannot be bypassed by the audit entry.
  await close.click();await page.locator("#retention-policy-open").click();await page.locator("#retention-policy-current").getByText("365 天",{exact:true}).waitFor();await page.locator("#retention-policy-days").fill("180");await page.locator("#retention-policy-reference").fill("CAB-PENDING");await page.locator("#retention-policy-confirm").check();await page.locator("#retention-policy-submit").click();await page.locator("#retention-policy-hint").getByText(/结果待确认/).waitFor();await page.locator("#retention-policy-close").click();count=calls.length;await entry.click();assert.equal(await page.locator("#audit-dialog").isVisible(),false);assert.equal(calls.length,count);assert.equal(await page.locator("#retention-policy-dialog").isVisible(),true);
  page.once("dialog",d=>d.accept());await page.locator("#retention-policy-abandon").click();await page.locator("#retention-policy-close").click();await entry.click();await hint.getByText(/已显示 20/).waitFor();
  mode="401";await more.click();await page.locator("#login-view").waitFor({state:"visible"});assert.equal(await cards.count(),0);assert.equal(await page.locator("#audit-dialog").isVisible(),false);assert.deepEqual(errors,[]);
  process.stdout.write("audit viewer filtering, lossless order, paging, access, timeout and identity races passed\n");
 }catch(err){process.stderr.write(err.stack+"\n");if(page)process.stderr.write(JSON.stringify({calls,writes,hint:await page.locator("#audit-hint").textContent().catch(()=>"missing")})+"\n");process.exitCode=1;}
 finally{for(const release of held)release();if(browser)await browser.close();server.closeAllConnections();server.close();}
});
