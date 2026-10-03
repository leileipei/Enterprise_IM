"use strict";
const assert = require("node:assert/strict"), http = require("node:http"), fs = require("node:fs"), path = require("node:path");
const { chromium } = require("playwright");
const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001", user = "00000000-0000-4000-8000-000000000002", admin = "00000000-0000-4000-8000-000000000003", employee = "00000000-0000-4000-8000-000000000004";
const rows = Array.from({ length: 23 }, (_, i) => ({ version: 23 - i, message_body_days: 123 - i, approval_reference: i === 21 ? "<img src=x onerror=alert(1)>CAB-2" : `CAB-HISTORY-${23-i}`, approved_by_user_id: user, approved_at: "2026-10-03T10:00:00.123456Z" }));
let current = { ...rows[0] }, port, mode = "", held, granted = true, empty = false, writes = 0, unauthorized = false;
const calls = [];
const server = http.createServer((req,res) => {
 const url = new URL(req.url, `http://127.0.0.1:${port}`);
 const send = (code,body,type="application/json") => { res.writeHead(code,{"Content-Type":type,"Cache-Control":"no-store"});res.end(typeof body === "string"?body:JSON.stringify(body)); };
 if (["/web/","/web/app.js","/web/retention.js","/web/legal-holds.js","/web/retention-policy.js","/web/retention-history.js", "/web/audit.js", "/web/message-search.js","/web/style.css"].includes(url.pathname)) {
  const name=url.pathname==="/web/"?"index.html":url.pathname.slice(5);if(!fs.existsSync(path.join(assets,name)))return send(404,{});
  return send(200,fs.readFileSync(path.join(assets,name),"utf8"),name.endsWith(".js")?"text/javascript":name.endsWith(".css")?"text/css":"text/html");
 }
 if(url.pathname==="/web/config")return send(200,{issuer:"https://sso.example.test/group",authorization_url:`http://127.0.0.1:${port}/authorize`,client_id:"enterprise-im-web",redirect_url:`http://127.0.0.1:${port}/web/`,scope:"openid profile"});
 if(url.pathname==="/authorize"){res.writeHead(302,{Location:`/web/?code=fixture&state=${url.searchParams.get("state")}`});return res.end();}
 if(url.pathname==="/web/oauth/token")return send(200,{access_token:"fixture-token",token_type:"Bearer",expires_in:300});
 if(req.headers.authorization!=="Bearer fixture-token"||unauthorized)return send(401,{error_code:"unauthorized"});
 if(url.pathname==="/api/v1/me")return send(200,{tenant_id:tenant,user_id:user,display_name:"管理员",global_employee_no:"A001",memberships:[{id:admin,organization_name:"集团总部",legal_entity_name:"总部法人",title:"管理员",is_primary:true},{id:employee,organization_name:"分公司",legal_entity_name:"分公司法人",title:"员工",is_primary:false}]});
 const actor=req.headers["x-acting-membership-id"];
 if(url.pathname.startsWith("/api/v1/admin/")&&(actor!==admin||!granted))return send(404,{error_code:"not_found"});
 if(url.pathname==="/api/v1/admin/retention-policy") {
  if(req.method==="GET")return send(200,current);
  assert.equal(req.method,"PUT");writes++;let raw="";req.on("data",b=>raw+=b);req.on("end",()=>{const body=JSON.parse(raw);current={...current,message_body_days:body.message_body_days,version:body.expected_version+1,approval_reference:body.approval_reference};rows.unshift({...current});send(200,current);});return;
 }
 if(url.pathname==="/api/v1/admin/retention-policy/history"){
  assert.equal(req.method,"GET");assert.equal(url.searchParams.get("limit"),"20");assert.deepEqual([...url.searchParams.keys()].sort(),url.searchParams.has("cursor")?["cursor","limit"]:["limit"]);
  const cursor=url.searchParams.get("cursor")||"";calls.push({actor,cursor});const fault=mode;mode="";
  if(["400","403","404","503","401"].includes(fault))return send(Number(fault),{error_code:"rejected"});
  let result={history:empty?[]:rows.filter(p=>!cursor||p.version<4).slice(0,20).map(p=>({...p,text:"NEVER_RENDER_BODY",content_digest:"NEVER_RENDER_DIGEST"})),next_cursor:!cursor&&!empty?"cursor-4":""};
  if(fault==="duplicate")result.history[1]={...result.history[0]};
  if(fault==="order")result.history.reverse();
  if(fault==="version-zero")result.history[0]={message_body_days:365,version:0,approval_reference:"",approved_by_user_id:"",approved_at:null};
  if(fault==="bad-date")result.history[0].approved_at="2026-02-30T10:00:00Z";
  if(fault==="cursor-loop")result.next_cursor=cursor;
  if(fault==="hold"){held=()=>send(200,result);return;}
  return send(200,result);
 }
 if(url.pathname==="/api/v1/conversations")return send(200,{conversations:[],has_more:false});
 if(url.pathname==="/api/v1/groups")return send(200,{groups:[],has_more:false});
 return send(404,{error_code:"not_found"});
});
server.listen(0,"127.0.0.1",async()=>{
 port=server.address().port;let browser,page;
 try {
  browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE||undefined});page=await browser.newPage({viewport:{width:1280,height:850}});page.setDefaultTimeout(3500);
  const errors=[];page.on("pageerror",e=>errors.push(e.message));
  await page.goto(`http://127.0.0.1:${port}/web/`);await page.locator("#login-button").click();await page.locator("#workspace").waitFor({state:"visible"});
  const actor=async name=>page.locator("#identity-options").getByRole("button",{name:new RegExp(name)}).click();await actor("集团总部");await page.locator("#retention-policy-open").click();await page.locator("#retention-policy-current").getByText("123 天",{exact:true}).waitFor();
  const toggle=page.locator("#retention-history-toggle"),cards=page.locator(".retention-history-card"),hint=page.locator("#retention-history-hint"),more=page.locator("#retention-history-more"),refresh=page.locator("#retention-history-refresh");
  assert.equal(await toggle.count(),1,"retention approval history entry must exist");
  await toggle.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(await cards.count(),20);assert.match(await cards.first().innerText(),/版本 23/);assert.equal(calls[0].actor,admin);assert.equal(calls[0].cursor,"");
  await more.click();await hint.getByText(/已显示 23/).waitFor();assert.equal(await cards.count(),23);assert.match(await cards.last().innerText(),/版本 1/);assert.equal(await more.isVisible(),false);assert.equal(calls[1].cursor,"cursor-4");
  assert.equal(await cards.locator("img").count(),0);assert.equal((await cards.allTextContents()).join("").includes("NEVER_RENDER"),false);assert.equal(writes,0);
  if(process.env.IM_TEST_HISTORY_SCREENSHOT_DIR){fs.mkdirSync(process.env.IM_TEST_HISTORY_SCREENSHOT_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.IM_TEST_HISTORY_SCREENSHOT_DIR,"desktop.png")});await page.setViewportSize({width:390,height:844});await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));await page.locator("#retention-history-section").scrollIntoViewIfNeeded();await page.screenshot({path:path.join(process.env.IM_TEST_HISTORY_SCREENSHOT_DIR,"mobile.png")});assert.equal(await page.locator("#retention-policy-dialog").evaluate(d=>d.scrollWidth<=d.clientWidth+1),true);await page.setViewportSize({width:1280,height:850});}
  await refresh.click();await hint.getByText(/已显示 20/).waitFor();mode="503";await more.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);assert.equal(await more.isVisible(),false);
  await refresh.click();await hint.getByText(/已显示 20/).waitFor();assert.equal(calls.at(-1).cursor,"");
  for(const fault of ["duplicate","order","version-zero","bad-date"]){mode=fault;await refresh.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);await refresh.click();await hint.getByText(/已显示 20/).waitFor();}
  mode="cursor-loop";await more.click();await hint.getByText(/加载失败/).waitFor();assert.equal(await cards.count(),0);
  empty=true;await refresh.click();await hint.getByText(/暂无变更审批/).waitFor();assert.equal(await cards.count(),0);empty=false;
  await refresh.click();await hint.getByText(/已显示 20/).waitFor();await toggle.click();assert.equal(await cards.count(),0);assert.equal(await page.locator("#retention-history-section").isVisible(),false);
  // Close while loading: late history never reappears.
  mode="hold";await toggle.click();await hint.getByText(/正在加载/).waitFor();await page.locator("#retention-policy-close").click();held();held=null;
  await page.locator("#retention-policy-open").click();await page.locator("#retention-policy-current").getByText("123 天",{exact:true}).waitFor();assert.equal(await cards.count(),0);
  // Mutation invalidates displayed/history requests; no stale history is accepted.
  mode="hold";await toggle.click();await hint.getByText(/正在加载/).waitFor();await page.locator("#retention-policy-days").fill("100");await page.locator("#retention-policy-reference").fill("CAB-HISTORY-NEW");await page.locator("#retention-policy-confirm").check();await page.locator("#retention-policy-submit").click();await page.locator("#retention-policy-hint").getByText(/服务器已确认/).waitFor();held();held=null;assert.equal(await cards.count(),0);assert.equal(await page.locator("#retention-history-section").isVisible(),false);
  await toggle.click();await hint.getByText(/已显示 20/).waitFor();assert.match(await cards.first().innerText(),/版本 24/);
  // Changed identity must discard late history.
  mode="hold";await refresh.click();await hint.getByText(/正在加载/).waitFor();await page.locator("#retention-policy-close").click();await actor("分公司");held();held=null;assert.equal(await cards.count(),0);await page.waitForFunction(()=>document.getElementById("retention-policy-open").classList.contains("hidden"));
  await actor("集团总部");await page.locator("#retention-policy-open").click();await page.locator("#retention-policy-current").getByText("100 天",{exact:true}).waitFor();await toggle.click();await hint.getByText(/已显示 20/).waitFor();
  for(const status of ["403","404"]){mode=status;await more.click();await page.locator("#retention-policy-hint").getByText(/权限已失效/).waitFor();assert.equal(await cards.count(),0);assert.equal(await page.locator("#retention-policy-submit").isDisabled(),true);assert.equal(await page.locator("#retention-policy-current").innerText(),"");await page.locator("#retention-policy-close").click();await actor("分公司");await actor("集团总部");await page.locator("#retention-policy-open").click();await page.locator("#retention-policy-current").getByText("100 天",{exact:true}).waitFor();await toggle.click();await hint.getByText(/已显示 20/).waitFor();}
  mode="401";await more.click();await page.locator("#login-view").waitFor({state:"visible"});assert.equal(await cards.count(),0);assert.equal(await page.locator("#retention-policy-dialog").isVisible(),false);
  assert.deepEqual(errors,[]);process.stdout.write("retention approval history paging, DTO validation, readonly rendering and identity races passed\n");
 }catch(err){process.stderr.write(err.stack+"\n");if(page)process.stderr.write(JSON.stringify({calls,writes,hint:await page.locator("#retention-history-hint").textContent().catch(()=>"missing")})+"\n");process.exitCode=1;}
 finally{if(held)held();if(browser)await browser.close();server.closeAllConnections();server.close();}
});
