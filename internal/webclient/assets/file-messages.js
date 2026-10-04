"use strict";
(() => {
 const validText=v => typeof v === "string" && !v.includes("\0") && new TextEncoder().encode(v).length <= 16384 && new TextDecoder("utf-8",{fatal:true}).decode(new TextEncoder().encode(v)) === v;
 const types=["application/pdf","image/png","image/jpeg","text/plain"];
 class FileMessages {
  #request;#context;#uuid;#canSend;#onPending;#onACK;#ready=null;#pending=null;#busy=false;#generation=0;#controller=null;
  constructor({request,context,uuidV7,canSend,onPending,onACK}) {this.#request=request;this.#context=context;this.#uuid=uuidV7;this.#canSend=canSend;this.#onPending=onPending;this.#onACK=onACK;}
  attach(ready) {
   if(this.#pending || this.#busy || !this.#canSend() || !window.FileTransport.validUUID(ready?.fileID) || !window.FileTransport.sameContext(ready.context,this.#context()))throw new Error("当前发送槽不可用");
   this.#ready=Object.freeze({fileID:ready.fileID,context:Object.freeze({...ready.context})});
  }
  contextChanged(){this.#generation++;this.#controller?.abort();this.#controller=null;this.#ready=null;this.#pending=null;this.#busy=false;this.#onPending?.(false);}
  async send(caption){
   if(this.#pending)throw new Error("请按原请求重试，不能修改待确认消息");
   if(!this.#ready || !this.#canSend() || !window.FileTransport.sameContext(this.#ready.context,this.#context()) || !validText(caption))throw new Error("附件或说明无效，说明最多16384字节");
   const id=this.#uuid();if(!/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id))throw new Error("消息编号无效");
   this.#pending=Object.freeze({context:this.#ready.context,body:Object.freeze({client_msg_id:id,message_type:"file",file_id:this.#ready.fileID,caption})});this.#onPending?.(true);return this.#submit();
  }
  async retry(){if(!this.#pending)throw new Error("没有待确认附件消息");return this.#submit();}
  async #submit(){
   if(this.#busy || !this.#pending)throw new Error("发送正在进行");
   const pending=this.#pending,generation=this.#generation,c=new AbortController();this.#controller=c;this.#busy=true;
   const check=()=>{if(generation!==this.#generation || !window.FileTransport.sameContext(pending.context,this.#context()) || c.signal.aborted){const e=new Error("发送上下文已变化");e.stale=true;throw e;}};
   const timer=setTimeout(()=>c.abort(),15000);let abort;const interrupted=new Promise((_,reject)=>{abort=()=>reject(new DOMException("发送结果待确认","AbortError"));c.signal.addEventListener("abort",abort,{once:true});});
   try{
    check();const resource=pending.context.kind==="group"?"groups":"conversations";
    const ack=await Promise.race([this.#request(`/api/v1/${resource}/${pending.context.conversation}/messages`,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(pending.body),signal:c.signal,credentials:"omit",cache:"no-store",redirect:"error"}),interrupted]);check();
    if(!ack || !window.FileTransport.validUUID(ack.message_id) || ack.conversation_id!==pending.context.conversation || !Number.isSafeInteger(ack.seq) || ack.seq<1 || !Number.isFinite(Date.parse(ack.server_time)) || typeof ack.duplicate!=="boolean")throw new Error("保存结果待确认，请按原编号核对");
    this.#pending=null;this.#ready=null;this.#onPending?.(false);await this.#onACK?.(ack);
   }finally{clearTimeout(timer);c.signal.removeEventListener("abort",abort);if(generation===this.#generation){this.#busy=false;this.#controller=null;}}
  }
  render(message){
   const card=document.createElement("div");card.className="file-message-card";
   if(message?.redacted===true){card.textContent="此消息当前不可见";return card;}
   if(!message || typeof message!=="object")throw new Error("消息响应无效");
   if(!message.message_type || message.message_type==="text"){if(!validText(message.text))throw new Error("消息正文无效");card.textContent=message.text;return card;}
   if(message.message_type!=="file" || !validText(message.caption))throw new Error("附件消息响应无效");
   const a=message.attachment;
   const available=a?.available===true && a.download_available===false && window.FileTransport.validUUID(a.file_id) && window.FileTransport.validFilename(a.original_filename) && typeof a.actual_size_bytes==="string" && /^[1-9][0-9]*$/.test(a.actual_size_bytes) && BigInt(a.actual_size_bytes)<=26214400n && types.includes(a.detected_media_type);
   if(!available){card.textContent="附件当前不可用";return card;}
   const name=document.createElement("strong");name.textContent=a.original_filename;
   const meta=document.createElement("p");meta.textContent=`${a.actual_size_bytes} 字节 · ${a.detected_media_type}`;
   const caption=document.createElement("p");caption.textContent=message.caption;card.append(name,meta,caption);card.setAttribute("data-file-id",a.file_id);return card;
  }
 }
 window.FileMessages=FileMessages;
})();
