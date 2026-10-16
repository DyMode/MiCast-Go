const {test}=require('node:test');
const assert=require('node:assert/strict');
require('../internal/server/web/qr.js');
function setup(states=[]){
 let starts=0,confirmed=0,hidden=0,clock=0;const timers=new Map(),signals=[];let seq=0;
 const flow=createMiCastQRFlow({now:()=>clock,setTimer:(fn,ms)=>{timers.set(++seq,{fn,ms});return seq},clearTimer:id=>timers.delete(id),show(){},hide(){hidden++},status(){},start:async signal=>{signals.push(signal);return{id:++starts,expires_at:new Date(clock+180000).toISOString()}},image:async()=>{},poll:async()=>{const next=states.shift()||{status:'waiting'};if(next instanceof Error)throw next;return next},confirmed:async()=>confirmed++});
 return {flow,signals,timers,get starts(){return starts},get confirmed(){return confirmed},get hidden(){return hidden},async tick(){const [id,t]=timers.entries().next().value;timers.delete(id);clock+=t.ms;await t.fn();for(let i=0;i<12;i++)await Promise.resolve();}};
}
test('expired QR renews automatically and confirmed login stops polling',async()=>{const s=setup([{status:'expired'},{status:'confirmed'}]);await s.flow.open();await s.tick();assert.equal(s.starts,2);assert.equal(s.signals[0].aborted,true);await s.tick();assert.equal(s.confirmed,1);assert.equal(s.timers.size,0)});
test('manual replacement and closing cancel previous requests',async()=>{const s=setup();await s.flow.open();await s.flow.open();assert.equal(s.starts,2);assert.equal(s.signals[0].aborted,true);assert.equal(s.timers.size,1);s.flow.close();assert.equal(s.signals[1].aborted,true);assert.equal(s.timers.size,0)});
test('403 retries the existing QR after a delay instead of requesting a new QR',async()=>{const s=setup([new Error('HTTP 403')]);await s.flow.open();await s.tick();assert.equal(s.starts,1);assert.equal([...s.timers.values()][0].ms,30000);s.flow.close()});
test('automatic replacement is bounded',async()=>{const s=setup(Array(5).fill({status:'expired'}));await s.flow.open();for(let i=0;i<4;i++)await s.tick();assert.equal(s.starts,4);assert.equal(s.timers.size,0)});
