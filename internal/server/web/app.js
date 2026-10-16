'use strict';
const $=id=>document.getElementById(id);
const base='/app/micast-go/';
let snapshot=null,entries=[],refreshing=null;
const expanded=new Set();
let qrObjectURL='',accessDirty=false,settingsDirty=false;
let accessState={required:false,authenticated:false,csrf:''},sessionToken='';
try{sessionToken=sessionStorage.getItem('micast-go-session')||'';}catch{}
function rememberSession(value){sessionToken=value||'';try{if(value)sessionStorage.setItem('micast-go-session',value);else sessionStorage.removeItem('micast-go-session');}catch{}}
async function loadAccess(){const response=await fetch(base+'api/access',{mode:'same-origin',cache:'no-store',headers:sessionToken?{Authorization:'Bearer '+sessionToken}:{}});if(!response.ok)throw new Error('无法读取管理登录设置');accessState=await response.json();return accessState;}

const accountNames={disconnected:'尚未连接',stored:'登录已保留',connected:'已连接',unstable:'连接暂不可用，登录已保留',expired:'登录已过期'};
const stateNames={stopped:'等待投送',playing:'正在播放',paused:'已暂停',transitioning:'正在准备',error:'播放失败',waiting_for_speakers:'等待音箱取流'};
function notice(text=''){$('notice').textContent=text;}
function el(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
async function api(path,method='GET',body,retried=false,signal){
 if(!accessState.csrf)await loadAccess();
 const response=await fetch(base+'api/'+path,{method,signal,mode:'same-origin',headers:{'X-MiCast-CSRF':accessState.csrf,...(sessionToken?{Authorization:'Bearer '+sessionToken}:{}),'X-MiCast-Request':'1',...(body?{'Content-Type':'application/json'}:{})},body:body?JSON.stringify(body):undefined});
 const data=await response.json();
 if(response.status===403&&!retried){await loadAccess();return api(path,method,body,true,signal);}
 if(response.status===401){$('workspace').hidden=true;$('access').hidden=false;$('lock').hidden=true;qrFlow.close();}
 if(!response.ok)throw new Error(data.error||'请求失败');return data;
}
async function action(button,fn){if(button.disabled)return;button.disabled=true;notice();try{await fn();}catch(error){notice(error.message);}finally{button.disabled=false;}}
function button(text,label,fn,cls='quiet'){
 const b=el('button',text,cls);b.type='button';b.title=label;b.setAttribute('aria-label',label);b.addEventListener('click',()=>action(b,fn));return b;
}
async function patchSpeaker(device,patch){await api('devices/'+encodeURIComponent(device.id),'PATCH',patch);await refresh(true);}
async function control(route,command,url){
 const data=await api('control','POST',{route,action:command,...(url?{url}:{})});
 const failures=Object.entries(data.results||{}).filter(([,result])=>result!=='accepted');
 if(failures.length)notice(failures.map(([id,result])=>(snapshot.devices.find(x=>x.id===id)?.name||id)+'：'+result).join('；'));
 await refresh(true);
}
function uiIcon(name){
 const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');svg.setAttribute('viewBox','0 0 24 24');svg.setAttribute('width','16');svg.setAttribute('height','16');svg.setAttribute('aria-hidden','true');svg.setAttribute('fill','none');svg.setAttribute('stroke','currentColor');svg.setAttribute('stroke-width','2');svg.setAttribute('stroke-linecap','round');svg.setAttribute('stroke-linejoin','round');const use=document.createElementNS(svg.namespaceURI,'use');use.setAttribute('href',base+'lucide.svg#'+name);svg.append(use);return svg;
}
function speakerIcon(muted){return uiIcon(muted?'volume-x':'volume-2');}
function groupIcon(){return uiIcon('house');}
function transportButtons(route,audio){
 const controls=el('div',undefined,'controls');
 if(!route||!audio||audio.state==='stopped')return controls;
 for(const [icon,label,command]of [[audio.state==='paused'?'play':'pause',audio.state==='paused'?'继续':'暂停',audio.state==='paused'?'play':'pause'],['square','停止','stop']]){const controlButton=button('',route.name+' '+label,()=>control(route.id,command));controlButton.append(uiIcon(icon));controls.append(controlButton);}
 return controls;
}
function renderWhole(force=false){
 const members=snapshot.devices.filter(x=>x.joined&&x.whole_house);
 $('whole-bar').hidden=members.length===0;
 if(!members.length){$('whole-bar').replaceChildren();return;}
 if(!force&&$('whole-bar').contains(document.activeElement))return;
 const route=entries.find(x=>x.id==='whole_house'),audio=route?.playback;
 const heading=el('div',undefined,'whole-heading');heading.append(el('strong','全屋播放'),el('span',members.length+' 台已加入','muted'));
 const playback=el('div',undefined,'session-playback');
 if(audio&&audio.state!=='stopped'){
  const title=[audio.track?.title,audio.track?.artist].filter(Boolean).join(' / ');
  playback.append(el('div',(audio.state==='paused'?'已暂停 · ':'')+(title||stateNames[audio.state]||''),'track-line playing'),transportButtons(route,audio));
  if(audio.error)playback.append(el('div',audio.error,'route-error'));
 }
 $('whole-bar').replaceChildren(heading,playback);
}
function powerIcon(){return uiIcon('power');}
function routeRow(route,device){
 const id='speaker:'+device.id,enabled=device.joined,audio=route?.playback||{state:'stopped',streams:{}};
 const actualOwner=entries.find(x=>(x.owned||[]).includes(device.id));
 const ours=actualOwner?.id===id&&audio.state!=='stopped';
 const item=el('article',undefined,'route-item'),row=el('div',undefined,'route-row'),info=el('div',undefined,'route-info');
 const heading=el('div',undefined,'route-title');heading.append(el('strong',device.name));

 if(!enabled)heading.append(el('span','入口已关闭','muted'));
 info.append(heading);
 const playback=el('div',undefined,'session-playback');
 if(ours){const title=[audio.track?.title,audio.track?.artist].filter(Boolean).join(' / ');playback.append(el('div',(audio.state==='paused'?'已暂停 · ':audio.state==='transitioning'?'准备中 · ':'')+(title||stateNames[audio.state]||''),'track-line playing'));}
 const protocolError=route&&[route.airplay,route.dlna].some(x=>x!=='ready');
 const error=(ours?audio.error:'')||device.error||(protocolError?'投送入口异常，请展开设置':'');if(error)info.append(el('div',error,'route-error'));
 const membership=el('label',undefined,'member-check'),check=el('input');check.type='checkbox';check.checked=enabled&&device.whole_house;check.disabled=!enabled;const membershipLabel=device.name+(check.checked?' 退出全屋播放':' 加入全屋播放');check.setAttribute('aria-label',membershipLabel);membership.title=membershipLabel;membership.classList.add('group-select');check.addEventListener('change',()=>{const previous=!check.checked;action(check,async()=>{try{await patchSpeaker(device,{whole_house:check.checked});}catch(error){check.checked=previous;throw error;}});});membership.append(check,groupIcon());
 const power=button('',device.name+' '+(enabled?'关闭投送入口':'开启投送入口'),()=>patchSpeaker(device,{joined:!enabled}),'power'+(enabled?' active':''));power.append(powerIcon());power.setAttribute('aria-pressed',String(enabled));
 const more=button('',device.name+(expanded.has(id)?' 收起设置':' 展开设置'),async()=>{expanded.has(id)?expanded.delete(id):expanded.add(id);renderRoutes(true);},'more-button');more.setAttribute('aria-expanded',String(expanded.has(id)));more.append(uiIcon('chevron-down'));
 row.append(info,membership,power,more);item.append(row);
 if(ours){playback.append(transportButtons(route,audio));item.append(playback);}
 const volume=el('div',undefined,'volume'),range=el('input'),out=el('output',String(device.volume));range.type='range';range.min='0';range.max='100';range.value=device.volume;range.disabled=!enabled;range.setAttribute('aria-label',device.name+' 音量');range.addEventListener('input',()=>{out.textContent=range.value;});range.addEventListener('change',()=>action(range,()=>patchSpeaker(device,{volume:Number(range.value)})));
 const mute=button('',device.name+(device.muted?' 取消静音':' 静音'),()=>patchSpeaker(device,{muted:!device.muted}),'mute'+(device.muted?' active':''));mute.append(speakerIcon(device.muted));mute.disabled=!enabled;mute.setAttribute('aria-pressed',String(device.muted));volume.append(mute,range,out);item.append(volume);
 if(expanded.has(id)){
  const options=el('div',undefined,'route-options'),formatLabel=el('label','输出格式'),format=el('select');for(const [value,text]of [['auto','默认'],['mp3','MP3'],['flac','FLAC'],['wav','WAV'],['pcm','PCM']])format.add(new Option(text,value));format.value=device.format;format.disabled=!enabled;format.setAttribute('aria-label',device.name+' 输出格式');format.addEventListener('change',()=>action(format,()=>patchSpeaker(device,{format:format.value})));formatLabel.append(format);
  const delayLabel=el('label','延迟（毫秒）'),delay=el('input');delay.type='number';delay.min='0';delay.max='5000';delay.step='10';delay.value=device.delay_ms;delay.disabled=!enabled;delay.setAttribute('aria-label',device.name+' 延迟毫秒');delay.addEventListener('change',()=>action(delay,()=>patchSpeaker(device,{delay_ms:Number(delay.value)})));delayLabel.append(delay);options.append(formatLabel,delayLabel);
  const detail=el('div',undefined,'connection-details');detail.append(el('span',device.hardware,'muted'));
  if(route?.airplay_address)detail.append(el('span','AirPlay '+route.airplay_address,'muted'));
  if(route?.description_url)detail.append(el('span','DLNA / 取流 '+new URL(route.description_url).host,'muted'));
  const bytes=(actualOwner?.playback.streams?.[device.id]?.bytes||0);if(bytes>0)detail.append(el('span',(bytes/1048576).toFixed(1)+' MiB','muted'));
  for(const protocol of ['airplay','dlna'])if(route?.[protocol]&&route[protocol]!=='ready')detail.append(el('span',protocol.toUpperCase()+'：'+route[protocol],'route-error'));
  options.append(detail);item.append(options);
 }
 return item;
}
function renderRoutes(force=false){
 if(!snapshot||(!force&&$('routes').contains(document.activeElement)))return;
 const byID=new Map(entries.map(x=>[x.id,x]));
 const rows=[];
 for(const device of snapshot.devices)rows.push(routeRow(byID.get('speaker:'+device.id),device));
 if(!snapshot.devices.length)rows.push(el('div',snapshot.account.user_id?'未发现音箱':'连接米家，开始播放','empty'));
 $('routes').replaceChildren(...rows);
}
function render(force=false){
 const data=snapshot;$('account-status').textContent=(accountNames[data.account.status]||data.account.status)+(data.account.error?' · '+data.account.error:'');$('account-dot').classList.toggle('connected',data.account.status==='connected');
 $('login').textContent='扫码登录';$('login').hidden=!['expired','disconnected'].includes(data.account.status)&&!!data.account.user_id;$('logout').disabled=!data.account.user_id;$('logout').hidden=!data.account.user_id;
 $('clear-entries').hidden=!(['expired','disconnected'].includes(data.account.status)&&data.devices.length);
 if(!accessDirty&&!document.querySelector('#access-settings :focus')){$('require-login').checked=accessState.required;$('current-password-label').hidden=!accessState.required;}
 $('new-password-label').hidden=!$('require-login').checked;
 $('summary').textContent='';$('route-count').textContent='· '+data.devices.length+' 台';
 if(!settingsDirty&&!document.querySelector('#settings-form :focus')){$('name').value=data.config.name;$('format').value=data.config.format;}
 const selection=$('play-route').value;if(document.activeElement!==$('play-route')){$('play-route').replaceChildren(...entries.map(x=>new Option(x.id==='whole_house'?'全屋播放 · '+x.name:x.name,x.id)));if(entries.some(x=>x.id===selection))$('play-route').value=selection;}
 renderWhole(force);
 renderRoutes(force);
}
async function refresh(force=false){
 if(refreshing){await refreshing;if(!force)return;}
 refreshing=(async()=>{const [data,routes]=await Promise.all([api('status'),api('routes'),loadAccess()]);snapshot=data;entries=routes;$('workspace').hidden=false;$('access').hidden=true;$('lock').hidden=!accessState.required;render(force);})();
 try{await refreshing;}finally{refreshing=null;}
}
$('access-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{const result=await api('access','POST',{key:$('access-key').value});rememberSession(result.session);await loadAccess();$('access-key').value='';await refresh(true);});});
const qrFlow=createMiCastQRFlow({
 start:signal=>api('account/qr','POST',undefined,false,signal),
 poll:(id,signal)=>api('account/qr/'+id+'/poll','POST',undefined,false,signal),
 image:async(qr,signal)=>{
  const response=await fetch(base+'api/account/qr/'+qr.id+'/image',{signal,mode:'same-origin',headers:sessionToken?{Authorization:'Bearer '+sessionToken}:{}});
  if(!response.ok)throw new Error('二维码读取失败');const data=await response.blob();if(signal.aborted)return;
  if(qrObjectURL)URL.revokeObjectURL(qrObjectURL);qrObjectURL=URL.createObjectURL(data);$('qr-image').src=qrObjectURL;
 },
 show:()=>{$('qr').hidden=false;},
 hide:()=>{$('qr').hidden=true;if(qrObjectURL){URL.revokeObjectURL(qrObjectURL);qrObjectURL='';$('qr-image').removeAttribute('src');}},
 status:text=>{$('qr-status').textContent=text;},
 confirmed:async()=>{try{await api('devices/refresh','POST');}catch(error){notice(error.message);}await refresh(true);}
});
for(const id of ['login','refresh-qr'])$(id).addEventListener('click',()=>action($(id),()=>qrFlow.open()));
$('cancel-qr').addEventListener('click',()=>qrFlow.close());
$('logout').addEventListener('click',()=>action($('logout'),async()=>{qrFlow.close();await api('account','DELETE');$('qr').hidden=true;await refresh(true);}));
$('refresh').addEventListener('click',()=>action($('refresh'),async()=>{await api('devices/refresh','POST');await refresh(true);}));
$('settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{await api('settings','PUT',{name:$('name').value,format:$('format').value});settingsDirty=false;await refresh(true);notice('设置已保存，入口已更新');});});
$('play-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{if(!$('play-route').value)throw new Error('请先开启一个投送入口');await control($('play-route').value,'play_url',$('audio-url').value.trim());$('command-result').textContent='正在准备播放…';});});
$('lock').addEventListener('click',()=>action($('lock'),async()=>{await api('access','DELETE');rememberSession('');qrFlow.close();$('qr').hidden=true;$('workspace').hidden=true;$('access').hidden=false;$('lock').hidden=true;}));
loadAccess().then(info=>{if(info.required&&!info.authenticated){$('access').hidden=false;return;}return refresh();}).catch(error=>notice(error.message));setInterval(()=>{if(!$('workspace').hidden)refresh().catch(error=>notice(error.message));},3000);

$('require-login').addEventListener('change',()=>{$('new-password-label').hidden=!$('require-login').checked;});
$('access-settings').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
 const result=await api('access','PUT',{enabled:$('require-login').checked,current_password:$('current-password').value,password:$('new-password').value});
 accessDirty=false;rememberSession(result.required?result.session:'');await loadAccess();$('current-password').value='';$('new-password').value='';await refresh(true);notice(result.required?'管理密码已保存':'已关闭管理登录');
});});
$('clear-entries').addEventListener('click',()=>action($('clear-entries'),async()=>{await api('devices','DELETE');await refresh(true);}));

$('settings-form').addEventListener('input',()=>{settingsDirty=true;});
$('access-settings').addEventListener('input',()=>{accessDirty=true;});

$('lock').append(uiIcon('log-out'));
