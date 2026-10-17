"use strict";
(() => {
  const uuid = v => typeof v === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(v);
  const stale = () => Object.assign(new Error("设备上下文已变化"), {stale:true});
  class DeviceSession {
    #api; #context; #uuid; #enabled; #device=null; #pending=null; #registration=null; #generation=0; #controller=null;
    constructor({apiCall,identityContext,uuidV7,enabled}) { this.#api=apiCall;this.#context=identityContext;this.#uuid=uuidV7;this.#enabled=enabled; }
    credential(context) {
      return this.#device && this.#device.accountKey===context.accountKey && Date.now()<this.#device.localExpiry ?
        Object.freeze({id:this.#device.id,token:this.#device.token}) : null;
    }
    async ensure(context) {
      if (!this.#enabled()) return null;
      if (!context?.accountKey || !context.membershipId) throw new Error("请先选择任职");
      if (this.#device && this.#device.accountKey!==context.accountKey) this.invalidate();
      const existing=this.credential(context);if(existing)return existing;
      if (this.#pending) { await this.#pending;return this.ensure(context); }
      const generation=this.#generation,controller=new AbortController();this.#controller=controller;
      const check=()=>{if(generation!==this.#generation || context.accountKey!==this.#context().accountKey || controller.signal.aborted)throw stale();};
      const run=async()=>{
        for(let attempt=0;attempt<2;attempt++) {
          check();if(!this.#registration)this.#registration=this.#uuid();
          let response;
          try {response=await this.#api("/api/v1/devices",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({registration_id:this.#registration,platform:"web",name:"Web 浏览器"}),signal:controller.signal},context);}
          catch(error) {
            check();
            if(error.code==="registration_exists" && uuid(error.deviceID)) {
              await this.#api(`/api/v1/devices/${error.deviceID}`,{method:"DELETE",signal:controller.signal},context,false);
              check();this.#registration=null;continue;
            }
            throw error;
          }
          check();const server=Date.parse(response?.server_time),expiry=Date.parse(response?.expires_at);
          if(!uuid(response?.device_id) || !/^[A-Za-z0-9_-]{43}$/.test(response?.device_token) || !Number.isFinite(server) || !Number.isFinite(expiry) || expiry<=server)throw new Error("设备登记响应无效");
          this.#device={id:response.device_id,token:response.device_token,accountKey:context.accountKey,context:Object.freeze({...context}),localExpiry:Date.now()+expiry-server};this.#registration=null;
          return this.credential(context);
        }
        throw new Error("登记结果待核对，请重试");
      };
      const promise=run();this.#pending=promise;
      try{return await promise;}finally{if(this.#pending===promise){this.#pending=null;this.#controller=null;}}
    }
    async list(cursor="") {
      if(!this.#enabled())return {devices:[],has_more:false};
      const context=this.#context(),generation=this.#generation,pending=this.#pending;
      // Include the device currently being registered. Quota failures still allow
      // listing old devices so the user can revoke one and retry registration.
      if(pending)try{await pending;}catch(error){if(error?.stale)throw error;}
      if(generation!==this.#generation || context.accountKey!==this.#context().accountKey)throw stale();
      return this.#api(`/api/v1/devices?limit=20${cursor?`&cursor=${encodeURIComponent(cursor)}`:""}`,{},context,false);
    }
    async revoke(id) {
      if(!uuid(id))throw new Error("设备编号无效");
      if(this.#device?.id===id)this.invalidate();
      await this.#api(`/api/v1/devices/${id}`,{method:"DELETE"},this.#context(),false);
    }
    cancelPending() {this.#generation++;this.#controller?.abort();this.#controller=null;this.#pending=null;}
    invalidate() {this.#generation++;this.#controller?.abort();this.#controller=null;this.#device=null;this.#pending=null;this.#registration=null;}
    async dispose() {
      const device=this.#device;this.invalidate();
      if(device)await this.#api(`/api/v1/devices/${device.id}`,{method:"DELETE",detached:true},device.context,false);
    }
  }
  window.DeviceSession=DeviceSession;
})();
