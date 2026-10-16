'use strict';
(() => {
 const dialog=document.getElementById('support-dialog'),body=document.getElementById('support-body'),title=document.getElementById('support-title');
 let request,previous;
 const link=(text,href)=>{const a=document.createElement('a');a.textContent=text;a.href=href;if(href.startsWith('https:')){a.target='_blank';a.rel='noopener noreferrer';}return a;};
 document.querySelectorAll('[data-support]').forEach(button=>button.addEventListener('click',async()=>{
  request?.abort();request=new AbortController();const signal=request.signal;previous=button;
  const kind=button.dataset.support;title.textContent=button.textContent;body.replaceChildren();dialog.showModal();
  if(kind==='contact'){
   body.append(link('support@dymode.com','mailto:support@dymode.com?subject=MiCast%20Go%20使用反馈'),link('MiCast 项目','https://github.com/DyMode/MiCast'));
   const p=document.createElement('p');p.textContent='反馈时请附上应用版本与问题步骤，移除密码和米家登录凭据。';body.append(p);
  }else if(kind==='donate'){
   const grid=document.createElement('div');grid.className='support-channels';
   for(const [name,file] of [['微信赞赏','wechat-appreciation.jpg'],['支付宝','alipay-support.jpg']]){
    const figure=document.createElement('figure'),caption=document.createElement('figcaption'),image=document.createElement('img');caption.textContent=name;image.src='/app/micast-go/'+file;image.alt=name+'收款图';image.width=240;image.height=340;figure.append(caption,image);grid.append(figure);
   }body.append(grid);const p=document.createElement('p');p.textContent='自愿支持开发，不影响功能使用。误付请联系 support@dymode.com。';body.append(p);
  }else{
   body.textContent='读取中…';
   try{const response=await fetch('/app/micast-go/'+kind+'.txt',{signal,mode:'same-origin'});if(!response.ok)throw Error();const content=await response.text();if(!signal.aborted){const article=document.createElement('div');article.className='legal-text';article.textContent=content;body.replaceChildren(article);}}
   catch(error){if(error.name!=='AbortError')body.textContent='读取失败，请关闭后重试。';}
  }
 }));
 document.getElementById('support-close').addEventListener('click',()=>dialog.close());
 dialog.addEventListener('close',()=>{request?.abort();body.replaceChildren();previous?.focus({preventScroll:true});});
 dialog.addEventListener('click',event=>{const r=dialog.getBoundingClientRect();if(event.target===dialog&&(event.clientX<r.left||event.clientX>r.right||event.clientY<r.top||event.clientY>r.bottom))dialog.close();});
})();
