"use strict";
const {chromium}=require("playwright");const assert=require("node:assert/strict");let stage="startup";
(async()=>{const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE||undefined});try{
const context=await browser.newContext({ignoreHTTPSErrors:true});const page=await context.newPage();
stage="login";await page.goto(process.env.IM_TEST_WEB_URL+"/web/");await page.getByRole("button",{name:/使用企业账号登录/}).click();
await page.locator("#workspace").waitFor({state:"visible",timeout:15000});assert.equal(new URL(page.url()).search,"");
stage="select identity and conversation";await page.locator("#identity-options .identity-button").first().click();await page.locator(".conversation-button").first().click();
stage="legacy placeholder";await page.getByText("附件消息（当前客户端不支持查看）",{exact:true}).waitFor({timeout:15000});
let body=await page.locator("body").textContent();assert(!body.includes(process.env.IM_TEST_PRIVATE_NAME));assert(!body.includes(process.env.IM_TEST_PRIVATE_CAPTION));
stage="text send";await page.locator("#message-text").fill("P424 real browser text");await page.locator("#send-button").click();await page.getByText("P424 real browser text",{exact:true}).waitFor({timeout:10000});
stage="reload and fresh PKCE login";await page.reload();await page.getByRole("button",{name:/使用企业账号登录/}).click();await page.locator("#workspace").waitFor({state:"visible",timeout:15000});await page.locator("#identity-options .identity-button").first().click();await page.locator(".conversation-button").first().click();
await page.getByText("附件消息（当前客户端不支持查看）",{exact:true}).waitFor({timeout:15000});await page.getByText("P424 real browser text",{exact:true}).waitFor({timeout:10000});
body=await page.locator("body").textContent();assert(!body.includes(process.env.IM_TEST_PRIVATE_NAME));assert(!body.includes(process.env.IM_TEST_PRIVATE_CAPTION));
assert.equal(await page.locator("a[download],button[data-action=download],a[href*=\"/content\"]").count(),0);
console.log(JSON.stringify({legacy_placeholder:true,text_send:true,reload_pull:true,private_values_hidden:true,download_entry_absent:true}));await context.close();
}finally{await browser.close()}})().catch(e=>{console.error(stage+": "+e.message);process.exit(1)});
