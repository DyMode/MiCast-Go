'use strict';
(function(root){
 root.createMiCastQRFlow=function(deps){
  const now=deps.now||Date.now,schedule=deps.setTimer||setTimeout,cancel=deps.clearTimer||clearTimeout;
  let generation=0,timer=null,controller=null,visible=false,renewals=0,failures=0;
  const alive=id=>visible&&generation===id;
  function clear(){if(timer!==null)cancel(timer);timer=null;if(controller)controller.abort();controller=null;}
  function close(){visible=false;generation++;clear();deps.hide();}
  function later(fn,delay,id){if(alive(id))timer=schedule(()=>{timer=null;if(alive(id))fn();},delay);}
  async function begin(manual=true){
   if(manual){renewals=0;failures=0;}
   else if(renewals++>=3){deps.status('自动更新已暂停，可点击重新获取');return;}
   clear();visible=true;const id=++generation;controller=new AbortController();const signal=controller.signal;
   deps.show();deps.status('正在获取二维码…');
   try{
    const qr=await deps.start(signal);if(!alive(id))return;
    await deps.image(qr,signal);if(!alive(id))return;
    const expires=Date.parse(qr.expires_at)||now()+180000;
    failures=0;deps.status('使用米家 App 扫码确认');
    async function poll(){
     if(!alive(id))return;
     if(now()>=expires){await begin(false);return;}
     try{
      const state=await deps.poll(qr.id,signal);if(!alive(id))return;
      if(state.status==='confirmed'){close();await deps.confirmed();return;}
      if(state.status==='expired'){await begin(false);return;}
      failures=0;deps.status(state.message||'使用米家 App 扫码确认');
      later(poll,Math.max(2500,Number(state.retry_after||0)*1000),id);
     }catch(error){if(alive(id)&&!signal.aborted)retry(error,poll,id);}
    }
    later(poll,2500,id);
   }catch(error){if(alive(id)&&!signal.aborted)retry(error,()=>begin(false),id);}
  }
  function retry(error,fn,id){
   failures++;
   if(failures>3){deps.status('暂时无法连接，请点击重新获取');return;}
   const base=/403|429|频繁|限流/.test(error.message)?30000:10000;
   const delay=Math.min(120000,base*2**(failures-1));
   deps.status((error.message||'连接失败')+'，'+delay/1000+' 秒后重试');later(fn,delay,id);
  }
  return {open:()=>begin(true),close};
 };
})(typeof window==='undefined'?globalThis:window);
