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
const stateNames={connected:'已连接，等待投送',stopped:'等待投送',playing:'正在播放',paused:'已暂停',transitioning:'正在准备',error:'播放失败',waiting_for_speakers:'等待音箱取流'};
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
const busyControls=new WeakSet();
async function action(button,fn){if(button.disabled||busyControls.has(button))return;busyControls.add(button);button.disabled=true;notice();try{await fn();}catch(error){notice(error.message);}finally{busyControls.delete(button);button.disabled=false;}}
function button(text,label,fn,cls='quiet'){
 const b=el('button',text,cls);b.type='button';b.title=label;b.setAttribute('aria-label',label);b.onclick=event=>action(event.currentTarget,fn);return b;
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
function playbackLabel(audio){const state=audio.activity||audio.state;if(state==='connected')return stateNames.connected;const title=[audio.track?.title,audio.track?.artist].filter(Boolean).join(' / ');return (state==='paused'?'已暂停 · ':state==='transitioning'?'准备中 · ':'')+(title||stateNames[state]||'');}
function transportButtons(route,audio){
 const controls=el('div',undefined,'controls');
 if(!route||!audio||audio.state==='stopped')return controls;
 for(const [icon,label,command]of [...((audio.activity||audio.state)==='connected'?[]:[[audio.state==='paused'?'play':'pause',audio.state==='paused'?'继续':'暂停',audio.state==='paused'?'play':'pause']]),['square','停止','stop']]){const controlButton=button('',route.name+' '+label,()=>control(route.id,command));controlButton.append(uiIcon(icon));controls.append(controlButton);}
 return controls;
}
// Keep controls alive across polling and metadata changes. New handlers close over
// the latest snapshot; input handlers act on the retained DOM node.
function nodeKey(node){return node.nodeType===1?(node.dataset.key||node.tagName+':'+(node.getAttribute('class')||'')): '#text';}
function reconcileChildren(parent, desired){
 const old=[...parent.childNodes],used=new Set();
 for(let i=0;i<desired.length;i++){
  const next=desired[i],key=nodeKey(next);
  const node=old.find(candidate=>!used.has(candidate)&&nodeKey(candidate)===key);
  if(node){used.add(node);patchNode(node,next);if(parent.childNodes[i]!==node)parent.insertBefore(node,parent.childNodes[i]||null);}
  else parent.insertBefore(next,parent.childNodes[i]||null);
 }
 for(const node of old)if(!used.has(node))node.remove();
}
function patchNode(node,next){
 if(node.nodeType===3){if(node.nodeValue!==next.nodeValue)node.nodeValue=next.nodeValue;return;}
 const editing=node===document.activeElement,busy=busyControls.has(node),nextValue=next.value,nextChecked=next.checked,nextDisabled=next.disabled;
 for(const attr of [...node.attributes])if(!next.hasAttribute(attr.name)&&!(attr.name==='disabled'&&busy))node.removeAttribute(attr.name);
 for(const attr of [...next.attributes])if(node.getAttribute(attr.name)!==attr.value&&!(attr.name==='disabled'&&busy))node.setAttribute(attr.name,attr.value);
 for(const event of ['onclick','onchange','oninput'])node[event]=next[event];
 if(node.tagName==='OUTPUT'){const range=node.parentElement.querySelector('input[type=range]');if(range&&(range===document.activeElement||busyControls.has(range)))return;}
 reconcileChildren(node,[...next.childNodes]);
 if('value' in node&&!editing&&node.value!==nextValue)node.value=nextValue;
 if('checked' in node&&!editing)node.checked=nextChecked;
 if('disabled' in node&&!busy)node.disabled=nextDisabled;
}
function renderWhole(force=false){
 const members=snapshot.devices.filter(x=>x.joined&&x.whole_house);
 $('whole-bar').hidden=members.length===0;
 if(!members.length){$('whole-bar').replaceChildren();return;}

 const route=entries.find(x=>x.id==='whole_house'),audio=route?.playback;
 const heading=el('div',undefined,'whole-heading');heading.append(el('strong','全屋播放'),el('span',members.length+' 台已加入','muted'));
 const playback=el('div',undefined,'session-playback');
 if(audio&&audio.state!=='stopped'){
  const title=[audio.track?.title,audio.track?.artist].filter(Boolean).join(' / ');
  playback.append(el('div',playbackLabel(audio),'track-line playing'),transportButtons(route,audio));
  if(audio.error)playback.append(el('div',audio.error,'route-error'));
 }
 reconcileChildren($('whole-bar'),[heading,playback]);
}
function powerIcon(){return uiIcon('power');}
function routeRow(route,device){
 const id='speaker:'+device.id,enabled=device.joined,audio=route?.playback||{state:'stopped',streams:{}};
 const actualOwner=entries.find(x=>(x.owned||[]).includes(device.id));
 const ours=audio.state!=='stopped'&&(actualOwner?.id===id||(!actualOwner&&audio.activity==='connected'));
 const item=el('article',undefined,'route-item'),row=el('div',undefined,'route-row'),info=el('div',undefined,'route-info');
 const heading=el('div',undefined,'route-title');heading.append(el('strong',device.name));

 if(!enabled)heading.append(el('span','入口已关闭','muted'));
 info.append(heading);
 const playback=el('div',undefined,'session-playback');
 if(ours){const title=[audio.track?.title,audio.track?.artist].filter(Boolean).join(' / ');playback.append(el('div',playbackLabel(audio),'track-line playing'));}
 const protocolError=route&&[route.airplay,route.dlna].some(x=>x!=='ready');
 const error=(ours?audio.error:'')||device.error||(protocolError?'投送入口异常，请展开设置':'');if(error)info.append(el('div',error,'route-error'));
 const membership=el('label',undefined,'member-check'),check=el('input');check.type='checkbox';check.checked=enabled&&device.whole_house;check.disabled=!enabled;const membershipLabel=device.name+(check.checked?' 退出全屋播放':' 加入全屋播放');check.setAttribute('aria-label',membershipLabel);membership.title=membershipLabel;membership.classList.add('group-select');check.onchange=event=>{const input=event.currentTarget,previous=!input.checked;action(input,async()=>{try{await patchSpeaker(device,{whole_house:input.checked});}catch(error){input.checked=previous;throw error;}});};membership.append(check,groupIcon());
 const power=button('',device.name+' '+(enabled?'关闭投送入口':'开启投送入口'),()=>patchSpeaker(device,{joined:!enabled}),'power'+(enabled?' active':''));power.append(powerIcon());power.setAttribute('aria-pressed',String(enabled));
 const more=button('',device.name+(expanded.has(id)?' 收起设置':' 展开设置'),async()=>{expanded.has(id)?expanded.delete(id):expanded.add(id);renderRoutes(true);},'more-button');more.setAttribute('aria-expanded',String(expanded.has(id)));more.append(uiIcon('chevron-down'));
 item.dataset.key=id;row.append(info,membership,power,more);item.append(row);
 if(ours){playback.append(transportButtons(route,audio));item.append(playback);}
 const displayedVolume=device.muted?device.volume:(device.current_volume??device.volume);
 const volume=el('div',undefined,'volume'),range=el('input'),out=el('output',String(displayedVolume));range.type='range';range.min='0';range.max='100';range.value=displayedVolume;range.disabled=!enabled;range.setAttribute('aria-label',device.name+' 音量');range.oninput=event=>{event.currentTarget.parentElement.querySelector('output').textContent=event.currentTarget.value;};range.onchange=event=>{const input=event.currentTarget;action(input,()=>patchSpeaker(device,{volume:Number(input.value)}));};
 const mute=button('',device.name+(device.muted?' 取消静音':' 静音'),()=>patchSpeaker(device,{muted:!device.muted}),'mute'+(device.muted?' active':''));mute.append(speakerIcon(device.muted));mute.disabled=!enabled;mute.setAttribute('aria-pressed',String(device.muted));volume.append(mute,range,out);item.append(volume);
 if(expanded.has(id)){
  const options=el('div',undefined,'route-options'),formatLabel=el('label','输出格式'),format=el('select');for(const [value,text]of [['auto','默认'],['mp3','MP3'],['flac','FLAC'],['wav','WAV'],['pcm','PCM']])format.add(new Option(text,value));format.value=device.format;format.disabled=!enabled;format.setAttribute('aria-label',device.name+' 输出格式');format.onchange=event=>{const input=event.currentTarget;action(input,()=>patchSpeaker(device,{format:input.value}));};formatLabel.append(format);
  const delayLabel=el('label','延迟（毫秒）'),delay=el('input');delay.type='number';delay.min='0';delay.max='5000';delay.step='10';delay.value=device.delay_ms;delay.disabled=!enabled;delay.setAttribute('aria-label',device.name+' 延迟毫秒');delay.onchange=event=>{const input=event.currentTarget;action(input,()=>patchSpeaker(device,{delay_ms:Number(input.value)}));};delayLabel.append(delay);options.append(formatLabel,delayLabel);
  const detail=el('div',undefined,'connection-details');detail.append(el('span',device.hardware,'muted'));
  const active=actualOwner?.playback?.protocol||audio?.protocol||'';
  if(active)detail.append(el('span',active==='airplay'?'输入 AirPlay':active==='dlna'?'输入 DLNA':'输入 链接','muted'));
  if(route?.airplay_address)detail.append(el('span','AirPlay 入口 '+route.airplay_address,'muted'));
  if(route?.description_url)detail.append(el('span','DLNA 入口 '+new URL(route.description_url).host,'muted'));
  const pull=actualOwner?.playback.streams?.[device.id];
  const bytes=pull?.bytes||0;if(bytes>0)detail.append(el('span','音箱取流 '+(bytes/1048576).toFixed(1)+' MiB','muted'));
  for(const protocol of ['airplay','dlna'])if(route?.[protocol]&&route[protocol]!=='ready')detail.append(el('span',protocol.toUpperCase()+'：'+route[protocol],'route-error'));
  const healthNames={flowing:'正在供流',observing:'连接暂时中断，正在观察',recovering:'正在恢复连接',unavailable:'连接未恢复',source_quiet:'音源暂时没有数据'};
  if(pull?.health)detail.append(el('span',healthNames[pull.health]||pull.health,'muted'));
  if(device.observed?.at){const fresh=Date.now()-Date.parse(device.observed.at)<90000;const text=device.observed.volume==null?'':'音箱回报音量 '+device.observed.volume+' · 保存音量 '+device.volume;const state={playing:'播放',paused:'暂停',stopped:'停止'}[device.observed.state]||'';detail.append(el('span',[text,state,fresh?'最近读取':'上次读取'].filter(Boolean).join(' · '),'muted'));}

  options.append(detail);item.append(options);
 }
 return item;
}
function renderRoutes(force=false){
 if(!snapshot)return;
 const byID=new Map(entries.map(x=>[x.id,x]));
 const rows=[];
 for(const device of snapshot.devices)rows.push(routeRow(byID.get('speaker:'+device.id),device));
 if(!snapshot.devices.length)rows.push(el('div',snapshot.account.user_id?'未发现音箱':'连接米家，开始播放','empty'));
 reconcileChildren($('routes'),rows);
}
function render(force=false){
 const data=snapshot;$('account-status').textContent=(accountNames[data.account.status]||data.account.status)+(data.account.error?' · '+data.account.error:'');$('account-dot').classList.toggle('connected',data.account.status==='connected');
 $('login').textContent='扫码登录';$('login').hidden=!['expired','disconnected'].includes(data.account.status)&&!!data.account.user_id;$('logout').disabled=!data.account.user_id;$('logout').hidden=!data.account.user_id;
 $('clear-entries').hidden=!(['expired','disconnected'].includes(data.account.status)&&data.devices.length);
 if(!accessDirty&&!document.querySelector('#access-settings :focus')){$('require-login').value=accessState.required?'on':'off';}
 updateAccessFields();
 $('summary').textContent='';$('route-count').textContent='· '+data.devices.length+' 台';
 if(!settingsDirty&&!document.querySelector('#settings-form :focus')){$('name').value=data.config.name;$('format').value=data.config.format;$('volume-mode').value=data.config.volume_mode||'independent';}
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
$('settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
 const accessChanged=accessDirty;
 if(accessChanged){
  const result=await api('access','PUT',{enabled:$('require-login').value==='on',current_password:$('current-password').value,password:$('new-password').value});
  accessDirty=false;rememberSession(result.required?result.session:'');await loadAccess();$('current-password').value='';$('new-password').value='';updateAccessFields();
 }
 try{
  await api('settings','PUT',{name:$('name').value,format:$('format').value,volume_mode:$('volume-mode').value,equalizer:{enabled:false,bass:0,mid:0,treble:0}});
 }catch(error){if(accessChanged)throw new Error('管理访问已保存，播放设置保存失败：'+error.message);throw error;}
 settingsDirty=false;await refresh(true);notice('已保存');
});});
$('play-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{if(!$('play-route').value)throw new Error('请先开启一个投送入口');await control($('play-route').value,'play_url',$('audio-url').value.trim());$('command-result').textContent='正在准备播放…';});});
$('lock').addEventListener('click',()=>action($('lock'),async()=>{await api('access','DELETE');rememberSession('');qrFlow.close();$('qr').hidden=true;$('workspace').hidden=true;$('access').hidden=false;$('lock').hidden=true;}));
loadAccess().then(info=>{if(info.required&&!info.authenticated){$('access').hidden=false;return;}return refresh();}).catch(error=>notice(error.message));setInterval(()=>{if(!$('workspace').hidden)refresh().catch(error=>notice(error.message));},3000);

function updateAccessFields(){
 const enabled=$('require-login').value==='on';
 $('new-password-label').hidden=!enabled;$('new-password').disabled=!enabled;
 $('new-password').required=enabled&&!accessState.required;
 $('current-password-label').hidden=!accessState.required;
 $('current-password').required=accessState.required&&accessDirty;
}
$('require-login').addEventListener('change',()=>{accessDirty=true;updateAccessFields();});
$('clear-entries').addEventListener('click',()=>action($('clear-entries'),async()=>{await api('devices','DELETE');await refresh(true);}));

$('settings-form').addEventListener('input',()=>{settingsDirty=true;});
$('access-settings').addEventListener('input',()=>{accessDirty=true;updateAccessFields();});

$('lock').append(uiIcon('log-out'));

$('diagnostics').addEventListener('click',()=>action($('diagnostics'),async()=>{const data=await api('diagnostics/export','POST');notice('诊断已写入应用文件：'+data.name);}));
