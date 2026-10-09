"use strict";
(() => {
  const parseSeq = value => {
    if(typeof value!=="string" || !/^(0|[1-9][0-9]*)$/.test(value) || value.length>19)throw new Error("消息编号无效");
    const number=BigInt(value);if(number>9223372036854775807n)throw new Error("消息编号越界");return number;
  };
  const same=(a,b)=>a?.accountKey===b?.accountKey && a?.identityKey===b?.identityKey && a?.conversation===b?.conversation && a?.conversationEpoch===b?.conversationEpoch && a?.receiptEpoch===b?.receiptEpoch;
  const stale=()=>Object.assign(new Error("回执上下文已变化"),{stale:true});
  class Receipts {
    #api;#context;#commit;#validate;#active;#show;#generation=0;#controllers=new Set();#after="0";#delivered=new Set();#read=new Set();#pending=new Set();#observed=new Map();#observer;#dialogObserver;#batchTimer=null;#reading=false;#readRetryDelay=1000;#onActivity;
    static parseSeq=parseSeq;
    get afterSeq(){return this.#after;}
    constructor({apiCall,identityContext,commitMessagePage,validateMessage,isConversationActive,showReceiptState}) {
      this.#api=apiCall;this.#context=identityContext;this.#commit=commitMessagePage;this.#validate=validateMessage;this.#active=isConversationActive;this.#show=showReceiptState;
      this.#observer=new IntersectionObserver(entries=>{for(const entry of entries){const record=this.#observed.get(entry.target);if(!record)continue;record.ratio=entry.intersectionRatio;this.#arm(record);}}, {threshold:[0,.5,1]});
      this.#onActivity=()=>{for(const record of this.#observed.values())this.#arm(record);};
      this.#dialogObserver=new MutationObserver(()=>{
        // Modal transitions interrupt continuous exposure even when geometry,
        // document visibility and window focus have not changed.
        for(const record of this.#observed.values()){clearTimeout(record.timer);record.timer=null;record.qualified=false;this.#pending.delete(record.seq);}
        this.#onActivity();
      });
      this.#dialogObserver.observe(document.body,{subtree:true,attributes:true,attributeFilter:["open"]});
      document.addEventListener("visibilitychange",this.#onActivity);window.addEventListener("blur",this.#onActivity);window.addEventListener("focus",this.#onActivity);
    }
    #check(context,generation){if(generation!==this.#generation || !same(context,this.#context()))throw stale();}
    #front(context){return same(context,this.#context()) && document.visibilityState==="visible" && document.hasFocus() && this.#active(context);}
    async #call(path,options,context,generation) {
      this.#check(context,generation);const c=new AbortController();this.#controllers.add(c);
      try {const response=await this.#api(path,{...options,signal:c.signal},context);this.#check(context,generation);return response;}
      finally{this.#controllers.delete(c);}
    }
    async acceptPage(page,context) {
      const generation=this.#generation;this.#check(context,generation);
      if(page?.conversation_id!==context.conversation || !Array.isArray(page.messages) || page.messages.length>500 || typeof page.has_more!=="boolean")throw new Error("消息同步响应无效");
      parseSeq(page.receipt_revision);let previous=parseSeq(this.#after);
      for(const message of page.messages){const seq=parseSeq(message?.seq);if(seq!==previous+1n)throw new Error("消息页不连续，请重新同步");previous=seq;this.#validate(message);}
      if(parseSeq(page.next_after_seq)!==previous || (page.has_more && !page.messages.length))throw new Error("消息页边界无效");
      const server=Date.parse(page.server_time),expiry=Date.parse(page.proof_expires_at);
      if(!Number.isFinite(server) || (page.messages.length && (!/^[A-Za-z0-9_-]{43}$/.test(page.sync_proof) || !Number.isFinite(expiry) || expiry<=server)) || (!page.messages.length && (page.sync_proof!==null || page.proof_expires_at!==null)))throw new Error("同步证明无效");
      // Validate and stage the complete page before the first receipt write.
      await this.#commit(page,context);this.#check(context,generation);
      if(page.sync_proof) {
        const state=await this.#call(`/api/v1/conversations/${context.conversation}/delivered`,{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({sync_proof:page.sync_proof})},context,generation);
        this.#validateState(state);if(parseSeq(state.scanned_seq)<previous)throw new Error("送达结果待核对");
        for(const message of page.messages)if(!message.redacted && message.sender_user_id!==context.userID)this.#delivered.add(message.seq);
        this.#show(state,context);
      }
      this.#after=page.next_after_seq;for(const record of this.#observed.values())this.#arm(record);
      return page.has_more;
    }
    #validateState(state){for(const key of ["scanned_seq","delivered_seq","read_seq","receipt_revision"])parseSeq(state?.[key]);if(typeof state.duplicate!=="boolean")throw new Error("回执响应无效");}
    observe(element,seq,context) {
      if(parseSeq(seq)===0n)throw new Error("消息编号无效");
      const old=this.#observed.get(element);if(old)clearTimeout(old.timer);
      this.#observed.set(element,{element,seq,context:Object.freeze({...context}),ratio:0,timer:null,qualified:false});this.#observer.observe(element);
    }
    forget(element) {
      const record=this.#observed.get(element);if(!record)return;clearTimeout(record.timer);this.#observer.unobserve(element);this.#observed.delete(element);this.#delivered.delete(record.seq);this.#pending.delete(record.seq);
    }
    #arm(record) {
      if(!record.element.isConnected || record.ratio<.5 || !this.#front(record.context) || !this.#delivered.has(record.seq) || this.#read.has(record.seq)){clearTimeout(record.timer);record.timer=null;record.qualified=false;return;}
      if(record.timer || record.qualified)return;
      const generation=this.#generation;
      record.timer=setTimeout(()=>{record.timer=null;if(generation===this.#generation && record.element.isConnected && record.ratio>=.5 && this.#front(record.context) && this.#delivered.has(record.seq)){record.qualified=true;this.confirmVisible([record.seq],record.context);}},500);
    }
    confirmVisible(seqs,context) {
      if(!this.#front(context))return;
      for(const seq of seqs)if(this.#delivered.has(seq) && !this.#read.has(seq) && [...this.#observed.values()].some(r=>r.seq===seq && r.qualified && r.element.isConnected && r.ratio>=.5 && same(r.context,context)))this.#pending.add(seq);
      if(this.#pending.size && !this.#batchTimer && !this.#reading)this.#batchTimer=setTimeout(()=>{this.#batchTimer=null;this.#flush(context).catch(()=>{/* A later visibility cycle or sync retries without inventing read facts. */});},300);
    }
    async #flush(context) {
      if(!this.#front(context) || this.#reading)return;
      const seqs=[...this.#pending].filter(seq=>[...this.#observed.values()].some(r=>r.seq===seq && r.qualified && r.element.isConnected && r.ratio>=.5)).sort((a,b)=>parseSeq(a)<parseSeq(b)?-1:1).slice(0,500);
      if(!seqs.length)return;const generation=this.#generation;this.#reading=true;
      let succeeded=false;
      try {
        const state=await this.#call(`/api/v1/conversations/${context.conversation}/read`,{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({seqs})},context,generation);this.#validateState(state);
        for(const seq of seqs){this.#read.add(seq);this.#pending.delete(seq);}succeeded=true;this.#readRetryDelay=1000;this.#show(state,context);
      } finally {if(generation===this.#generation){this.#reading=false;for(const r of this.#observed.values())this.#arm(r);if(this.#pending.size){const delay=succeeded?300:this.#readRetryDelay;this.#readRetryDelay=Math.min(this.#readRetryDelay*2,30000);this.#batchTimer=setTimeout(()=>{this.#batchTimer=null;this.#flush(context).catch(()=>{});},delay);}}}
    }
    contextChanged() {
      this.#generation++;for(const c of this.#controllers)c.abort();this.#controllers.clear();
      for(const r of this.#observed.values())clearTimeout(r.timer);this.#observer.disconnect();this.#observed.clear();clearTimeout(this.#batchTimer);this.#batchTimer=null;
      this.#after="0";this.#delivered.clear();this.#read.clear();this.#pending.clear();this.#reading=false;
    }
    dispose(){this.contextChanged();this.#dialogObserver.disconnect();document.removeEventListener("visibilitychange",this.#onActivity);window.removeEventListener("blur",this.#onActivity);window.removeEventListener("focus",this.#onActivity);}
  }
  window.Receipts=Receipts;
})();
