"use strict";
(() => {
 class FileDownload {
  #transport;#context;#capabilities;#container;#controller=null;#url=null;#link=null;#timer=null;#generation=0;#busy=false;
  constructor({transport,context,capabilities,saveContainer}){this.#transport=transport;this.#context=context;this.#capabilities=capabilities;this.#container=saveContainer;}
  #release(){if(this.#timer!==null)clearTimeout(this.#timer);this.#timer=null;if(this.#url)URL.revokeObjectURL(this.#url);this.#url=null;this.#link=null;this.#container.replaceChildren();}
  contextChanged(){this.#generation++;this.#controller?.abort();this.#controller=null;this.#busy=false;this.#release();}
  save(){if(!this.#link || !this.#url)throw new Error("完整文件已过期，请重新请求下载");this.#link.click();this.#release();}
  async request(fileID){
   if(this.#busy || this.#url)throw new Error("请先保存或等待当前文件链接过期");
   if(this.#capabilities()?.download_enabled!==true || !window.FileTransport.validUUID(fileID) || typeof URL.createObjectURL!=="function" || typeof URL.revokeObjectURL!=="function")throw new Error("当前不能保存此文件");
   const origin=Object.freeze({...this.#context()}),generation=this.#generation,c=new AbortController();this.#controller=c;this.#busy=true;
   const timer=setTimeout(()=>c.abort(),65000);let abort;const interrupted=new Promise((_,reject)=>{abort=()=>reject(new DOMException("下载已取消","AbortError"));c.signal.addEventListener("abort",abort,{once:true});});
   const hint=document.createElement("p");hint.textContent="正在完整读取文件；完成前不会出现保存链接。";hint.setAttribute("role","status");this.#container.replaceChildren(hint);
   try{
    const result=await Promise.race([this.#transport.readDownload(fileID,c.signal),interrupted]);
    if(c.signal.aborted || generation!==this.#generation || !window.FileTransport.sameContext(origin,this.#context()) || this.#capabilities()?.download_enabled!==true)throw new Error("下载上下文已变化");
    if(!(result?.blob instanceof Blob) || result.blob.size<1 || result.blob.size>26214400 || !window.FileTransport.validFilename(result.filename))throw new Error("下载结果无效");
    this.#url=URL.createObjectURL(result.blob);
    const link=document.createElement("a");link.href=this.#url;link.download=result.filename;link.textContent="保存到本机";link.setAttribute("data-file-save","");
    link.addEventListener("click",()=>{const url=this.#url;setTimeout(()=>{if(url && this.#url===url)this.#release();},0);});
    this.#link=link;hint.textContent="文件已完整读取，请在60秒内保存到本机。已保存的本机文件不受后续撤权影响。";this.#container.replaceChildren(hint,link);this.#timer=setTimeout(()=>this.#release(),60000);
   }catch(e){if(generation===this.#generation){this.#release();const failed=document.createElement("p");failed.textContent="下载未完成；没有可保存的完整文件，请重新请求。";failed.setAttribute("role","status");this.#container.append(failed);}throw e;}
   finally{clearTimeout(timer);c.signal.removeEventListener("abort",abort);if(generation===this.#generation){this.#busy=false;this.#controller=null;}}
  }
 }
 window.FileDownload=FileDownload;
})();
