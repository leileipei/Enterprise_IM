"use strict";
const http=require("node:http"),fs=require("node:fs"),path=require("node:path"),assert=require("node:assert/strict");
const {chromium}=require("playwright");
const assets=path.join(__dirname,"..","assets"),tenant="00000000-0000-4000-8000-000000000001",user="00000000-0000-4000-8000-000000000002",primary="00000000-0000-4000-8000-000000000003",secondary="00000000-0000-4000-8000-00000000000a",direct="00000000-0000-4000-8000-000000000005",group="00000000-0000-4000-8000-000000000011";
const goTrim=s=>s.replace(/^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/gu, "");
const simpleLower=s=>[...s].map(c=>c==="İ"?"i":c.toLowerCase()).join("");
const rows=Array.from({length:21},(_,i)=>({id:`00000000-0000-4000-8000-${(8100+i).toString().padStart(12,"0")}`,seq:String(9007199254740991n+BigInt(i)),sender_user_id:user,text:i===0?"<img src=x onerror=alert(1)> 工单首条":"工单记录 "+i,server_time:"2026-10-03T10:00:00.123456Z"}));
rows.forEach((r,i)=>{r.conversation_id=i<20?direct:group;r.conversation_kind=i<20?"direct":"group";if(i===20)r.seq="1";});
let port,mode="",emptyFirst=false,cycle=false,writes=0;const calls=[],held=[];
const server=http.createServer((req,res)=>{
 const url=new URL(req.url,`http://127.0.0.1:${port}`);
 const send=(code,value,type="application/json")=>{if(res.destroyed)return;res.writeHead(code,{"Content-Type":type,"Cache-Control":"no-store"});res.end(typeof value==="string"?value:JSON.stringify(value));};
 if(url.pathname==="/web/"||/^\/web\/[a-z-]+\.(js|css)$/.test(url.pathname)){
  const name=url.pathname==="/web/"?"index.html":url.pathname.slice(5);const file=path.join(assets,name);if(!fs.existsSync(file))return send(404,{});return send(200,fs.readFileSync(file,"utf8"),name.endsWith("js")?"text/javascript":name.endsWith("css")?"text/css":"text/html");
 }
 if(url.pathname==="/web/config")return send(200,{issuer:"https://sso.example.test/group",authorization_url:`http://127.0.0.1:${port}/authorize`,client_id:"enterprise-im-web",redirect_url:`http://127.0.0.1:${port}/web/`,scope:"openid profile"});
 if(url.pathname==="/authorize"){res.writeHead(302,{Location:`/web/?code=fixture&state=${url.searchParams.get("state")}`});return res.end();}
 if(url.pathname==="/web/oauth/token")return send(200,{access_token:"mock-token",token_type:"Bearer",expires_in:300});
 if(req.headers.authorization!=="Bearer mock-token")return send(401,{error_code:"unauthorized"});
 if(url.pathname==="/api/v1/me")return send(200,{tenant_id:tenant,user_id:user,display_name:"测试人员",global_employee_no:"A001",memberships:[{id:primary,organization_name:"集团总部",legal_entity_name:"总部法人",title:"工程师",is_primary:true},{id:secondary,organization_name:"子公司",legal_entity_name:"子公司法人",title:"顾问",is_primary:false}]});
 const actor=req.headers["x-acting-membership-id"];if(![primary,secondary].includes(actor))return send(403,{error_code:"invalid_identity"});
 if(req.method!=="GET"&&url.pathname!=="/api/v1/realtime/tickets"){writes++;return send(405,{});}
 if(url.pathname.startsWith("/api/v1/admin/"))return send(404,{error_code:"not_found"});
 if(url.pathname==="/api/v1/realtime/tickets")return send(404,{error_code:"not_found"});
 if(url.pathname==="/api/v1/conversations")return send(200,{conversations:[{id:direct,type:"direct",last_seq:1,updated_at:"2026-10-03T10:00:00Z",peer_visible:true,display_name:"已有同事",organization_name:"集团总部"}],has_more:false});
 if(url.pathname==="/api/v1/groups")return send(200,{groups:[{id:group,type:"group",name:"策略暂停群",status:"policy_blocked",role:"member",source_membership_id:secondary,last_seq:1,updated_at:"2026-10-03T10:00:00Z"}],has_more:false});

 if(url.pathname==="/api/v1/messages/search"){
  const q=url.searchParams.get("q"),kind=url.searchParams.get("kind"),cursor=url.searchParams.get("cursor")||"";
  assert.equal(req.method,"GET");assert.equal(url.searchParams.get("limit"),"20");assert.equal([...url.searchParams.keys()].every(k=>["q","kind","cursor","limit"].includes(k)),true);
  calls.push({kind,q,cursor,actor});const fault=mode;mode="";
  if(["400","401","403","503","identity403"].includes(fault))return send(fault==="identity403"?403:Number(fault),{error_code:fault==="identity403"?"invalid_identity":"rejected"});
  let page={messages:[],has_more:false,next_cursor:""};
  if(cycle){page.next_cursor=cursor==="A"?"B":"A";page.has_more=true;}
  else if(emptyFirst&&!cursor){page.next_cursor="empty-scan";page.has_more=true;}
  else if(simpleLower(goTrim(q))==="工单"){
   const selected=rows.filter(r=>kind==="all"||kind===r.conversation_kind),offset=cursor==="page-20"?20:0;page.messages=selected.slice(offset,offset+20).map(r=>({...r}));page.has_more=offset+20<selected.length;page.next_cursor=page.has_more?"page-20":"";
  }else if(goTrim(q)==="\uFEFF工单"){page.messages=[{...rows[0],text:"\uFEFF工单"}];}
  else if(simpleLower(goTrim(q))==="iabc"||simpleLower(goTrim(q))==="οσ"){page.messages=[{...rows[0],text:simpleLower(goTrim(q))}];}
  if(fault==="bad-conversation")page.messages[0].conversation_id="bad";
  if(fault==="bad-kind")page.messages[0].conversation_kind="channel";
  if(fault==="wrong-kind")page.messages[0]={...rows[20]};
  if(fault==="numeric")page.messages[0].seq=9007199254740992;
  if(fault==="overflow")page.messages[0].seq="9223372036854775808";
  if(fault==="leading")page.messages[0].seq="01";
  if(fault==="duplicate")page.messages[1]={...page.messages[0]};
  if(fault==="order")page.messages.reverse();
  if(fault==="conversation-order")page.messages=[{...rows[20]},{...rows[0]}];
  if(fault==="bad-time")page.messages[0].server_time="2026-02-30T10:00:00Z";
  if(fault==="bad-id")page.messages[0].id="bad";
  if(fault==="bad-sender")page.messages[0].sender_user_id="bad";
  if(fault==="wrong-match")page.messages[0].text="不相干正文";
  if(fault==="redacted")page.messages[0].redacted=true;
  if(fault==="huge-text")page.messages[0].text="工单"+"字".repeat(6000);
  if(fault==="bad-more")page.has_more=false;
  if(fault==="long-cursor")page.next_cursor="x".repeat(2049);
  if(fault==="too-many")page.messages=rows.map(r=>({...r}));
  if(fault==="replay"){page.messages=[{...rows[0]}];page.next_cursor="";page.has_more=false;}
  if(fault==="extra-name")page.messages[0].conversation_name="禁止推断的名称";
  if(fault==="hold"||fault==="timeout"){held.push(()=>send(200,page));return;}
  return send(200,page);
 }
 if(url.pathname.endsWith("/messages"))return send(200,{conversation_id:url.pathname.split("/")[4],messages:[{seq:1,sender_user_id:user,text:"已有消息",server_time:"2026-10-03T10:00:00Z"}],next_after_seq:1,has_more:false});
 return send(404,{error_code:"not_found"});
});

server.listen(0,"127.0.0.1",async()=>{
 port=server.address().port;let browser,page;
 try{
  browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE||undefined});page=await browser.newPage({viewport:{width:1280,height:850}});page.setDefaultTimeout(4000);const errors=[];page.on("pageerror",e=>errors.push(e.message));
  await page.addInitScript(()=>{const timeout=AbortSignal.timeout.bind(AbortSignal);AbortSignal.timeout=ms=>timeout(ms===15000?500:ms);const original=window.fetch;window.fetch=(url,opts)=>{if(window.ignoreSearchAbort&&String(url).includes("/api/v1/messages/search")){opts={...opts};delete opts.signal;}return original(url,opts);};});
  await page.goto(`http://127.0.0.1:${port}/web/`);await page.locator("#login-button").click();await page.locator("#workspace").waitFor({state:"visible"});
  const entry=page.locator("#cross-message-search-open");assert.equal(await entry.count(),1,"cross history search entry must exist");assert.equal(await entry.isVisible(),false);
  await page.locator("#identity-options").getByRole("button",{name:/集团总部/}).click();assert.equal(await entry.isVisible(),true);assert.equal(await page.evaluate(()=>activeConversation),null);
  await entry.click();
  const dialog=page.locator("#cross-message-search-dialog"),q=page.locator("#cross-message-search-query"),kind=page.locator("#cross-message-search-kind"),apply=page.locator("#cross-message-search-apply"),more=page.locator("#cross-message-search-more"),refresh=page.locator("#cross-message-search-refresh"),hint=page.locator("#cross-message-search-hint"),cards=page.locator(".cross-message-search-card"),close=page.locator("#cross-message-search-close");
  const submit=async(query="工单")=>{await q.fill(query);await apply.click();};
  assert.equal(calls.length,0);assert.equal(await kind.inputValue(),"all");await submit("  工单  ");await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).q,"工单");assert.equal(calls.at(-1).kind,"all");assert.equal(await cards.count(),20);assert.equal(await cards.locator("img").count(),0);assert.match(await cards.nth(2).innerText(),/9007199254740993/);assert.match(await cards.first().innerText(),/<img src=x/);assert.match(await cards.first().innerText(),new RegExp(direct));
  await more.click();await hint.getByText(/已显示 21/).waitFor();assert.equal(await cards.count(),21);assert.match(await cards.last().innerText(),/群聊/);assert.match(await cards.last().innerText(),/消息序号 1/);assert.equal(await more.isVisible(),false);assert.equal(await page.locator("#messages").getByText("工单首条").count(),0);
  const beforeKind=calls.length;await kind.selectOption("group");assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);assert.equal(calls.length,beforeKind);await apply.click();await hint.getByText(/已显示 1 条/).waitFor();assert.equal(calls.at(-1).kind,"group");assert.match(await cards.first().innerText(),new RegExp(group));
  await kind.selectOption("direct");mode="wrong-kind";await apply.click();await hint.getByText(/搜索失败/).waitFor();assert.equal(await cards.count(),0);await kind.selectOption("all");
  emptyFirst=true;await refresh.click();await hint.getByText(/本页未找到.*继续/).waitFor();assert.equal(await cards.count(),0);await more.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).cursor,"empty-scan");emptyFirst=false;
  for(const bad of [""," ","字","字".repeat(101),"ab\0"]){const n=calls.length;await submit(bad);await hint.getByText(/关键词/).waitFor();assert.equal(calls.length,n);assert.equal(await cards.count(),0);}
  await submit("\u0085工单\u0085");await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).q,"工单");
  await submit("\uFEFF工单");await hint.getByText(/已显示 1 条/).waitFor();assert.equal(calls.at(-1).q,"\uFEFF工单");
  for(const query of ["İABC","ΟΣ"]){await submit(query);await hint.getByText(/已显示 1/).waitFor();assert.equal(await cards.count(),1);}
  await submit("无匹配");await hint.getByText(/未找到.*匹配/).waitFor();assert.equal(await more.isVisible(),false);
  for(const fault of ["bad-conversation","bad-kind","numeric","overflow","leading","duplicate","order","conversation-order","bad-time","bad-id","bad-sender","wrong-match","redacted","huge-text","bad-more","long-cursor","too-many"]){mode=fault;await submit();await hint.getByText(/搜索失败/).waitFor();assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);}
  mode="extra-name";await submit();await hint.getByText(/已显示 20/).waitFor();assert.equal((await cards.allTextContents()).some(t=>t.includes("禁止推断")),false);
  mode="replay";await more.click();await hint.getByText(/搜索失败/).waitFor();assert.equal(await cards.count(),0);
  cycle=true;await refresh.click();await hint.getByText(/本页未找到/).waitFor();await more.click();await hint.getByText(/本页未找到/).waitFor();await more.click();await hint.getByText(/搜索失败/).waitFor();cycle=false;
  for(const code of ["400","503","403"]){mode=code;await refresh.click();await hint.getByText(/搜索失败|无权限/).waitFor();assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);}
  await page.evaluate(()=>{window.ignoreSearchAbort=true;});mode="hold";await refresh.click();await hint.getByText(/正在搜索/).waitFor();await q.fill("无匹配");await apply.click();await hint.getByText(/未找到.*匹配/).waitFor();const keywordHint=await hint.innerText();held.shift()();await page.waitForTimeout(80);assert.equal(await cards.count(),0);assert.equal(await hint.innerText(),keywordHint);
  mode="hold";await submit();await hint.getByText(/正在搜索/).waitFor();await kind.selectOption("group");await apply.click();await hint.getByText(/已显示 1 条/).waitFor();const groupSnapshot=await cards.allTextContents(),groupHint=await hint.innerText();held.shift()();await page.waitForTimeout(80);assert.deepEqual(await cards.allTextContents(),groupSnapshot);assert.equal(await hint.innerText(),groupHint);
  mode="hold";await refresh.click();await hint.getByText(/正在搜索/).waitFor();await page.evaluate(id=>selectMembership(id),secondary);assert.equal(await dialog.isVisible(),false);assert.equal(await q.inputValue(),"");assert.equal(await kind.inputValue(),"all");await entry.click();await submit();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).actor,secondary);const identitySnapshot=await cards.allTextContents();held.shift()();await page.waitForTimeout(80);assert.deepEqual(await cards.allTextContents(),identitySnapshot);
  mode="hold";await refresh.click();await hint.getByText(/正在搜索/).waitFor();await close.click();held.shift()();await page.waitForTimeout(80);assert.equal(await cards.count(),0);assert.equal(await q.inputValue(),"");await page.evaluate(()=>{window.ignoreSearchAbort=false;});await entry.click();mode="timeout";await submit();await hint.getByText(/搜索失败/).waitFor();assert.equal(await cards.count(),0);held.shift()();
  await refresh.click();await hint.getByText(/已显示 20/).waitFor();
  if(process.env.IM_TEST_CROSS_SEARCH_SCREENSHOT_DIR){const dir=process.env.IM_TEST_CROSS_SEARCH_SCREENSHOT_DIR;fs.mkdirSync(dir,{recursive:true});await page.screenshot({path:path.join(dir,"desktop.png")});await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(dir,"mobile-form.png")});await cards.first().scrollIntoViewIfNeeded();await page.screenshot({path:path.join(dir,"mobile-results.png")});assert.equal(await dialog.evaluate(d=>d.scrollWidth<=d.clientWidth+1),true);await page.setViewportSize({width:1280,height:850});}
  mode="identity403";await refresh.click();await dialog.waitFor({state:"hidden"});assert.equal(await cards.count(),0);assert.equal(await entry.isVisible(),false);await page.locator("#identity-options").getByRole("button",{name:/集团总部/}).click();await entry.click();await submit();await hint.getByText(/已显示 20/).waitFor();mode="401";await refresh.click();await page.locator("#login-view").waitFor({state:"visible"});assert.equal(await cards.count(),0);assert.equal(await q.inputValue(),"");
  assert.deepEqual(errors,[]);assert.equal(writes,0);console.log("cross history search entry, filters, paging, precision, Unicode, DTO, access, timeout and late-response isolation passed");
 }catch(e){console.error(e);process.exitCode=1;}finally{if(browser)await browser.close();server.close();}
});
