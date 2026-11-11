'use strict';
const assert = require('node:assert/strict');
const {create} = require('../assets/auth-view.js');
function fixture(api) {
 const pending = new Map(), values = new Map(), progress = [], errors = []; let sequence = 0;
 const clock = {setTimeout(fn,delay) {const id=++sequence;pending.set(id,{fn,delay});return id;},clearTimeout(id){pending.delete(id);}};
 const storage={setItem(k,v){values.set(k,v);},getItem(k){return values.get(k)||null;},removeItem(k){values.delete(k);}};
 const monitor=create({api,clock,storage,onProgress:async v=>progress.push(v),onError:(e,terminal)=>errors.push({e,terminal})});
 return {monitor,pending,values,progress,errors,storage,async tick(){const [id,item]=pending.entries().next().value;pending.delete(id);await item.fn();}};
}
(async()=>{
 const calls=[];let status={flow_id:'original',profile_id:'profile',state:'waiting_authorization',flow:{id:'next-stage',url:'https://accounts.example/next'}};
 const f=fixture(async(route,method,body)=>{calls.push({route,method,body});return status;});
 await f.monitor.track({id:'original',auto_complete:true},'profile');
 assert.equal(calls[0].route,'/api/auth/status');assert.equal(calls[0].body.flow_id,'original');
 assert.equal(f.progress[0].flow.url,status.flow.url);
 assert.ok(!JSON.stringify([...f.values]).includes('accounts.example'),'persist only tracking identifiers, never authorization URLs');
 status={...status,state:'waiting_login'};await f.tick();
 assert.equal(f.progress.at(-1).state,'waiting_login');
 status={...status,state:'ready',mountpoint:'/automatic'};await f.tick();
 assert.equal(f.pending.size,0);assert.equal(f.values.size,0);assert.equal(f.progress.at(-1).mountpoint,'/automatic');
 assert.ok(calls.every(c=>c.route==='/api/auth/status'),'monitor must never require a complete/mount/create-directory call');
 // Page reopening resumes backend-owned work using the original tracking ID.
 const resumed=fixture(async()=>({flow_id:'persisted',state:'ready'}));
 resumed.storage.setItem('drivebridge:pending-auth',JSON.stringify({flowID:'persisted',profileID:'profile'}));
 await resumed.monitor.restore();assert.equal(resumed.progress[0].flow_id,'persisted');
 // Transport failures are retried; requests and stale replies never overlap current progress.
 let release;let count=0;
 const g=fixture(async()=>{count++;if(count===1)throw new Error('connection');return new Promise(resolve=>{release=resolve;});});
 await g.monitor.track({id:'one',auto_complete:true},'profile');assert.equal(g.errors[0].terminal,false);assert.ok(g.pending.size);
 const next=g.monitor.checkNow();const duplicate=g.monitor.checkNow();assert.equal(count,2);
 release({flow_id:'one',state:'ready'});await Promise.all([next,duplicate]);assert.equal(g.pending.size,0);
 const denied=fixture(async()=>{throw Object.assign(new Error('gone'),{status:404});});
 await denied.monitor.track({id:'gone',auto_complete:true},'profile');assert.equal(denied.errors[0].terminal,true);assert.equal(denied.values.size,0);
 let oldRelease;
 const switched=fixture(async(route,method,body)=>body.flow_id==='old' ? new Promise(resolve=>{oldRelease=resolve;}) : {flow_id:'new',state:'ready'});
 const old=switched.monitor.track({id:'old',auto_complete:true},'a');
 await switched.monitor.track({id:'new',auto_complete:true},'b');
 oldRelease({flow_id:'old',state:'ready'});await old;
 assert.deepEqual(switched.progress.map(p=>p.flow_id),['new'],'a late old account response must not overwrite current progress');
 console.log('automatic auth monitoring, stage handoff, restoration, deduplication and terminal errors verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
