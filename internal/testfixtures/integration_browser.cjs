'use strict';
// Observe actual local Playwright child processes; keep its native launch/pipe
// and download APIs. Lifecycle messages use dedicated inherited pipes only.
const fs = require('node:fs');
const cp = require('node:child_process');
const {AsyncLocalStorage} = require('node:async_hooks');
const writer = Number(process.env.IM_BROWSER_LIFECYCLE_WRITE_FD);
const reader = Number(process.env.IM_BROWSER_LIFECYCLE_ACK_FD);
if (writer !== 3 || reader !== 4) throw new Error('browser_lifecycle_pipe_invalid');
const emit = (event,pid,actual_exit) => fs.writeSync(writer,JSON.stringify({event,pid,...(actual_exit?{actual_exit}:{})})+'\n');
function ack(expected) {
  let line='';const byte=Buffer.alloc(1);
  while (line.length<100) {
    if (fs.readSync(reader,byte,0,1,null)!==1) throw new Error('browser_registration_missing');
    if (byte[0]===10) {if(line!==expected)throw new Error('browser_registration_mismatch');return;}
    line+=byte.toString();
  }
  throw new Error('browser_registration_invalid');
}
emit('node_ready',process.pid);ack('node_registered');
const executable=fs.realpathSync(process.env.CHROMIUM_EXECUTABLE);
const launches=new AsyncLocalStorage(), children=new Set(), browsers=new Set();
const originalSpawn=cp.spawn;
cp.spawn=function(command,args,options) {
  const state=launches.getStore();
  let selected=false;
  if (state) {try{selected=fs.realpathSync(command)===executable;}catch(_){}}
  const child=originalSpawn.apply(this,arguments);
  if (selected) {
    if (state.child) throw new Error('browser_launch_multiple_children');
    state.child=child;children.add(child);
    child.once('spawn',()=>{emit('chrome_started',child.pid);ack('chrome_registered:'+child.pid);});
    child.once('exit',(code,signal)=>{
      children.delete(child);
      const names={SIGKILL:'killed',SIGTERM:'terminated',SIGINT:'interrupt'};
      emit('chrome_exited',child.pid,signal?'signal:'+(names[signal]||signal):'exit:'+code);
    });
  }
  return child;
};
const {chromium}=require('playwright');
const originalLaunch=chromium.launch.bind(chromium);
chromium.launch=function(options) {
  return launches.run({child:null},async()=>{
    const state=launches.getStore(),browser=await originalLaunch(options);
    if (!state.child) throw new Error('actual_browser_process_missing');
    browsers.add(browser);browser.once('disconnected',()=>browsers.delete(browser));
    emit('chrome_ready',state.child.pid);return browser;
  });
};
let interrupting=false;
process.on('SIGINT',()=>{
  if (interrupting) return;interrupting=true;
  (async()=>{
    await Promise.all([...browsers].map(browser=>browser.close().catch(()=>{})));
    await Promise.all([...children].map(child=>new Promise(resolve=>{
      if(child.exitCode!==null||child.signalCode!==null)return resolve();
      child.once('exit',resolve);child.kill('SIGTERM');
      const timer=setTimeout(()=>{if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');},1000);timer.unref();
    })));
    process.exit(130);
  })().catch(()=>process.exit(131));
});
