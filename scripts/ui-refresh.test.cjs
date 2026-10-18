const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
let playwright;
try{playwright=require('playwright');}catch{}

test('metadata refresh retains controls, focus and current event handlers',{skip:!playwright},async()=>{
 const browser=process.env.UI_TEST_CHANNEL?await playwright.chromium.launch({headless:true,channel:process.env.UI_TEST_CHANNEL}):await playwright.webkit.launch({headless:true,executablePath:process.env.UI_TEST_EXECUTABLE||undefined});
 try{
  const page=await browser.newPage();
  page.setDefaultTimeout(15000);
  const errors=[];
  page.on('pageerror',error=>errors.push(error.message));
  const root=path.join(__dirname,'../internal/server/web');
  const html=fs.readFileSync(path.join(root,'index.html'),'utf8').replace(/<script\b[^>]*>[\s\S]*?<\/script>/g,'').replace(/<link\b[^>]*>/g,'');
  await page.route('**/*',route=>route.abort());
  await page.setContent(html,{waitUntil:'domcontentloaded'});
  await page.addStyleTag({content:fs.readFileSync(path.join(root,'style.css'),'utf8')});
  await page.evaluate(()=>{
   window.createMiCastQRFlow=()=>({close(){},open(){}});
   window.requests=[];window.mockAccess={required:false,authenticated:true,csrf:'test'};
   window.mockStatus={account:{status:'connected',user_id:'test'},config:{name:'MiCast Go',format:'wav',volume_mode:'independent'},devices:[{id:'speaker',name:'Living room',joined:true,whole_house:true,volume:20,format:'auto',delay_ms:0}]};
   window.mockRoutes=[{id:'speaker:speaker',name:'Living room',owned:['speaker'],airplay:'ready',dlna:'ready',playback:{state:'playing',track:{title:'First line'},streams:{}}},{id:'whole_house',name:'Home',owned:[],playback:{state:'playing',track:{title:'Whole line'},streams:{}}}];
   window.fetch=async(url,options={})=>{
    const endpoint=url.split('/api/')[1];
    if(endpoint==='access'&&options.method==='PUT'){const patch=JSON.parse(options.body);requests.push({access:patch});mockAccess.required=patch.enabled;return {ok:true,status:200,json:async()=>({...mockAccess,session:'test-session'})};}
    if(endpoint==='settings'&&options.method==='PUT'){const patch=JSON.parse(options.body);requests.push(patch);Object.assign(mockStatus.config,patch);}
    if(options.method==='PATCH'){
     const patch=JSON.parse(options.body);requests.push(patch);Object.assign(mockStatus.devices[0],patch);
    }
    return {ok:true,status:200,json:async()=>endpoint==='access'?mockAccess:endpoint==='status'?mockStatus:endpoint==='routes'?mockRoutes:{results:{}}};
   };
   // Tests trigger refresh explicitly.
   window.setInterval=()=>0;
  });
  await page.addScriptTag({content:fs.readFileSync(path.join(root,'app.js'),'utf8')});
  await page.waitForFunction(()=>document.querySelector('.route-item'));
  const retained=await page.evaluate(()=>{
   window.originalButtons=[...document.querySelectorAll('#routes button,#whole-bar button')];
   window.originalRange=document.querySelector('#routes input[type=range]');
   for(let i=0;i<20;i++){
    entries[0].playback.track.title='Lyric '+i;
    entries[1].playback.track.title='Whole lyric '+i;
    render();
   }
   return originalButtons.every((node,i)=>node===[...document.querySelectorAll('#routes button,#whole-bar button')][i])&&originalRange===document.querySelector('#routes input[type=range]');
  });
  assert.equal(retained,true);
  await page.evaluate(()=>{
   originalRange.focus();originalRange.value='67';originalRange.dispatchEvent(new Event('input'));
   render();
  });
  assert.deepEqual(await page.evaluate(()=>[document.activeElement===originalRange,originalRange.value,originalRange.parentElement.querySelector('output').textContent]),[true,'67','67']);
  await page.evaluate(()=>{originalRange.dispatchEvent(new Event('change'));});
  await page.waitForFunction(()=>requests.some(x=>x.volume===67));
  await page.waitForFunction(()=>!document.querySelector('#routes input[type=range]').disabled);
  // An update must install handlers for the newest snapshot, not detached nodes.
  await page.evaluate(()=>{mockStatus.devices[0]={...mockStatus.devices[0],muted:true};snapshot=mockStatus;render();});
  await page.locator('#routes .mute').click();
  await page.waitForFunction(()=>requests.some(x=>x.muted===false));
  assert.equal(await page.locator('#routes .more-button').isVisible(),true);
  await page.locator('#routes .more-button').click({force:true});
  assert.equal(await page.locator('#routes select').inputValue(),'auto');
  await page.evaluate(()=>{render();});
  assert.equal(await page.locator('#routes select').inputValue(),'auto');
  assert.equal(await page.locator('#volume-mode').inputValue(),'independent');
  await page.locator('#settings-form').evaluate(form=>{form.closest('details').open=true;});
  await page.locator('#volume-mode').selectOption('linked');
  await page.evaluate(()=>{render();});
  assert.equal(await page.locator('#volume-mode').inputValue(),'linked');
  assert.equal(await page.locator('.equalizer-settings').count(),0);
  await page.locator('#settings-form button').click({force:true});
  await page.waitForFunction(()=>requests.some(p=>p.volume_mode==='linked'&&p.equalizer?.enabled===false));
  await page.evaluate(()=>{mockStatus.devices[0].current_volume=71;mockStatus.devices[0].observed={volume:71,state:'paused',at:new Date().toISOString()};mockStatus.devices[0].format='pcm';render();});
  assert.match(await page.locator('.connection-details').innerText(),/71/);
  assert.equal(await page.locator('#routes input[type=range]').inputValue(),'71');
  assert.equal(await page.locator('#routes select').inputValue(),'pcm');
  assert.doesNotMatch(await page.locator('.connection-details').innerText(),/兼容性|尝试|尚未确认/);
  await page.evaluate(()=>{delete mockStatus.devices[0].observed;mockStatus.devices[0].volume=30;document.activeElement.blur();render();});
  assert.equal(await page.locator('#routes input[type=range]').inputValue(),'71');
  await page.evaluate(()=>{mockRoutes[0].owned=[];mockRoutes[0].playback.activity='connected';render();});
  assert.match(await page.locator('#routes .track-line').innerText(),/已连接，等待投送/);
  assert.equal(await page.locator('#routes .controls button').count(),1);
  await page.evaluate(()=>{mockRoutes[0].owned=['speaker'];mockRoutes[0].playback.activity='playing';render();});
  assert.equal(await page.locator('#routes .controls button').count(),2);
  if(process.env.UI_TEST_SCREENSHOT)await page.screenshot({path:process.env.UI_TEST_SCREENSHOT,fullPage:true});
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),true);
  if(process.env.UI_TEST_SCREENSHOT)await page.screenshot({path:process.env.UI_TEST_SCREENSHOT.replace('.png','-mobile.png'),fullPage:true});
  assert.equal(await page.locator('#settings-form button').count(),1);
  await page.locator('#require-login').selectOption('on');
  assert.equal(await page.locator('#new-password').isVisible(),true);
  assert.equal(await page.locator('#new-password').getAttribute('required'),'');
  await page.locator('#new-password').fill('password123');
  await page.locator('#settings-form button').click();
  await page.waitForFunction(()=>requests.some(p=>p.access?.enabled===true));
  await page.waitForFunction(()=>!document.querySelector('#settings-form button').disabled);
  await page.locator('#require-login').selectOption('off');
  assert.equal(await page.locator('#new-password').isVisible(),false);
  assert.equal(await page.locator('#current-password').isVisible(),true);
  await page.locator('#current-password').fill('password123');
  await page.locator('#settings-form button').click();
  await page.waitForFunction(()=>requests.some(p=>p.access?.enabled===false&&p.access.current_password==='password123'));
  await page.waitForFunction(()=>!document.querySelector('#settings-form button').disabled);
  assert.equal(await page.locator('#current-password').isVisible(),false);
  assert.deepEqual(errors,[]);
 }finally{await browser.close();}
});
