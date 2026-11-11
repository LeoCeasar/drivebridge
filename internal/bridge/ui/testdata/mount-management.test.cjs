'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
class Element{constructor(){this.children=[];this.listeners={};this.hidden=true;this.value='';this.classList={toggle(){}};this.button={disabled:false};}append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}querySelector(){return this.button;}}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const calls=[];let mount={id:'mount',name:'Account',archived:false,background_pull_excluded_extensions:['.log','.db'],state:{running:true,local_saved:true,queued:3,conflicts:1,cloud_state:'conflict'}};
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:{},DriveBridgeDiscovery:{create:()=>({setProfiles(){},load:async()=>{}})},addEventListener(){}},fetch:async(route,options)=>{
 const body=options.body?JSON.parse(options.body):null;calls.push({route,method:options.method,body});
 let result={};
 if(route==='/api/config'){
  if(options.method==='POST'){assert.equal(body.action,'mount_policy');mount.background_pull_excluded_extensions=body.background_pull_excluded_extensions;}
  result={platform:'darwin',profiles:[],remotes:[],mounts:[mount]};
 }else if(route==='/api/status')result={mounts:[mount],conflicts:[]};
 else if(route==='/api/mounts/mount/archive'){mount={...mount,archived:true,state:{...mount.state,running:false}};}
 else if(route==='/api/mounts/mount/restore'){mount={...mount,archived:false};}
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
 const active=el('mount-rows').children[0];assert.equal(active.children[4].children[0].textContent,'停止同步');
 await active.children[4].children[1].listeners.click();
 assert.equal(el('mount-rows').children.length,0);assert.equal(el('archived-mount-panel').hidden,false);
 const archived=el('archived-mount-rows').children[0];assert.match(archived.children[3].textContent,/队列 3.*冲突 1/);
 await archived.children[4].children[0].listeners.click();
 assert.equal(el('archived-mount-panel').hidden,true);assert.equal(el('mount-rows').children.length,1);
 assert.equal(mount.state.running,false,'restore only restores configuration');
 assert.ok(!calls.some(c=>c.route.endsWith('/start')||c.method==='DELETE'),'archive/restore never starts uploads or deletes cloud content');
 console.log('editable suffix defaults, empty rules, local mount archive and safe restore verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
