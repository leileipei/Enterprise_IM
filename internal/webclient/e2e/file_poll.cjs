"use strict";
const {chromium}=require("playwright"),assert=require("node:assert/strict"),fs=require("node:fs/promises");
const {waitPageFlag}=require("./file_browser_poll.cjs");let browser;
process.on("SIGINT",()=>browser?.close().finally(()=>process.exit(130)));
(async()=>{const input=JSON.parse(await fs.readFile(process.env.WEB_FILE_INPUT,"utf8"));browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});try{const context=await browser.newContext({ignoreHTTPSErrors:true}),page=await context.newPage();await page.goto(input.baseURL+"/web/");assert.equal(await waitPageFlag(page,"/__p425/poll-proof","ready"),true);await context.close();console.log(JSON.stringify({actual_false_then_true_awaited:true}));}finally{await browser.close();}})().catch(()=>{console.error("real boolean poll returned before actual true");process.exitCode=1;});
