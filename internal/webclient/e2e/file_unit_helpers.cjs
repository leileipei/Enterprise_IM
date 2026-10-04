'use strict';
const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path'),{webcrypto}=require('node:crypto');
class Element {
 constructor(tag='div'){this.tagName=tag.toUpperCase();this.children=[];this.value='';this.disabled=false;this.hidden=false;this.checked=false;this.open=false;this.listeners={};this.classes=new Set();this.classList={add:(...v)=>v.forEach(x=>this.classes.add(x)),remove:(...v)=>v.forEach(x=>this.classes.delete(x)),toggle:(x,on)=>{on=on===undefined?!this.classes.has(x):on;on?this.classes.add(x):this.classes.delete(x);return on},contains:x=>this.classes.has(x)};this.attributes={};this._text='';}
 set textContent(v){this._text=String(v);this.children=[]}get textContent(){return this._text+this.children.map(c=>typeof c==='string'?c:c.textContent).join('')}
 append(...items){this.children.push(...items)}appendChild(v){this.append(v);return v}replaceChildren(...v){this._text='';this.children=v}remove(){this.removed=true}focus(){}showModal(){this.open=true}close(){this.open=false}click(){this.fire('click',{preventDefault(){}})}setAttribute(k,v){this.attributes[k]=String(v)}addEventListener(k,f){(this.listeners[k]??=[]).push(f)}removeEventListener(k,f){this.listeners[k]=(this.listeners[k]||[]).filter(x=>x!==f)}fire(k,e={}){for(const f of this.listeners[k]||[])f(e)}querySelectorAll(tag){return this.children.flatMap(c=>typeof c==='string'?[]:[...(c.tagName===tag.toUpperCase()?[c]:[]),...c.querySelectorAll(tag)])}
}
function environment(name,dependencies=[]){
 const elements=new Map(),document=new Element();document.hidden=false;document.createElement=tag=>new Element(tag);document.getElementById=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)};
 let now=Date.now(),id=0;const timers=new Map(),window={},DateMock=class extends Date{static now(){return now}};
 const context={window,document,TextEncoder,TextDecoder,AbortController,AbortSignal,DOMException,Headers,Blob,Uint8Array,URL,URLSearchParams,crypto:webcrypto,Date:DateMock,queueMicrotask,setTimeout:(f,ms)=>{const n=++id;timers.set(n,{f,ms,at:now+ms});return n},clearTimeout:n=>timers.delete(n)};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../assets/file-transport.js'),'utf8'),context);
 for(const dependency of dependencies)vm.runInContext(fs.readFileSync(path.join(__dirname,'../assets/'+dependency+'.js'),'utf8'),context);
 vm.runInContext(fs.readFileSync(process.env.FILE_UNIT_SOURCE,'utf8'),context);
 const flush=async()=>{for(let i=0;i<20;i++)await Promise.resolve()};
 const tick=async(ms)=>{const end=now+ms;for(let n=0;n<1000;n++){const next=[...timers.entries()].filter(([,t])=>t.at<=end).sort((a,b)=>a[1].at-b[1].at)[0];if(!next)break;now=next[1].at;timers.delete(next[0]);next[1].f();await flush()}now=end;await flush()};
 return {window,document,elements,timers,context,tick,flush,now:()=>now,Element};
}
module.exports={environment,Element};
