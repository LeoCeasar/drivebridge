'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
class Element{constructor(){this.children=[];this.listeners={};this.hidden=true;this.value='';this.classList={toggle(){}};this.button={disabled:false};}append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}querySelector(){return this.button;}}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const calls=[];let removed=false;let mount={id:'mount',name:'Account',archived:false,background_pull_excluded_extensions:['.log','.db'],state:{running:true,local_saved:true,queued:3,conflicts:1,cloud_state:'conflict'}};
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:{},DriveBridgeDiscovery:{create:()=>({setProfiles(){},load:async()=>{}})},addEventListener(){}},fetch:async(route,options)=>{
 const body=options.body?JSON.parse(options.body):null;calls.push({route,method:options.method,body});
 let result={};
 if(route==='/api/config'){
  if(options.method==='POST'){assert.equal(body.action,'mount_policy');mount.background_pull_excluded_extensions=body.background_pull_excluded_extensions;}
  result={platform:'darwin',profiles:[],remotes:[],mounts:removed?[]:[mount]};
 }else if(route==='/api/status')result={mounts:removed?[]:[mount],conflicts:[]};
 else if(route==='/api/mounts/mount/delete'){
  if(mount.state.queued||mount.state.conflicts)return{ok:false,status:409,json:async()=>({code:'mount_unsynced',message:'未同步，禁止删除'})};
  removed=true;
 }
 else if(route==='/api/mounts/mount/restore_start'){mount={...mount,archived:false,state:{...mount.state,running:true}};}
 else throw new Error('unexpected side effect '+route);
 return{ok:true,status:200,json:async()=>result};
}};
const source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
vm.runInNewContext(source.replace('  start();','  globalThis.loadConfig=loadConfig;globalThis.refresh=refresh;'),context);
(async()=>{
 await context.loadConfig();await context.refresh();
 assert.equal(el('pull-policy-extensions').value,'.log, .db');
 el('pull-policy-extensions').value='.log, .sqlite';await el('pull-policy-form').listeners.submit({preventDefault(){}});
 assert.deepEqual(mount.background_pull_excluded_extensions,['.log','.sqlite']);
 el('pull-policy-extensions').value='';await el('pull-policy-form').listeners.submit({preventDefault(){}});
 assert.deepEqual(mount.background_pull_excluded_extensions,[],'clearing the input explicitly disables defaults');
 const active=el('mount-rows').children[0];assert.equal(active.children[4].children[0].textContent,'暂停同步');
 await active.children[4].children[1].listeners.click();
 assert.equal(el('mount-rows').children.length,1,'blocked removal must leave the mapping visible');assert.equal(removed,false);
 assert.ok(el('message').children.some(e=>e.children.some(b=>b.textContent==='处理冲突')),'blocked removal needs a direct conflict action');
 mount={...mount,archived:true,state:{...mount.state,running:false}};await context.refresh();
 const archived=el('archived-mount-rows').children[0];assert.match(archived.children[3].textContent,/队列 3.*冲突 1/);
 assert.equal(archived.children[4].children[0].textContent,'恢复并启动同步');
 await archived.children[4].children[0].listeners.click();
 assert.equal(el('archived-mount-panel').hidden,true);assert.equal(el('mount-rows').children.length,1);
 assert.equal(mount.state.running,true,'recovery action must also start sync');
 mount={...mount,state:{...mount.state,queued:0,conflicts:0}};await context.refresh();
 await el('mount-rows').children[0].children[4].children[1].listeners.click();
 assert.equal(el('mount-rows').children.length,0);assert.equal(removed,true);
 assert.ok(!calls.some(c=>c.route.endsWith('/archive')),'new deletion must not silently archive');
 console.log('suffix settings, blocked deletion with help, restore-and-start, and actual mapping deletion verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
