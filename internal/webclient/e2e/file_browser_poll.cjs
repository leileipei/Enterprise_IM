"use strict";
const {expect}=require("playwright/test");
async function waitPageFlag(page,path,key,timeout=30000){
 let value=false;
 await expect.poll(async()=>{
  value=await page.evaluate(async({path,key})=>{const r=await fetch(path,{cache:"no-store"});if(r.status!==200)throw new Error("Private condition unavailable");return (await r.json())[key]===true;},{path,key});
  return value;
 },{timeout,intervals:[20,50,100,200]}).toBe(true);
 return value;
}
module.exports={waitPageFlag};
