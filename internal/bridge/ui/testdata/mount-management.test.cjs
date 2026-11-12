'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
class Element{constructor(){this.children=[];this.listeners={};this.hidden=true;this.value='';this.classList={toggle(){}};this.button={disabled:false};}append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}querySelector(){return this.button;}}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const calls=[];let mount={id:'mount',name:'账号盘-c143343f-4a52-4999-b7b0-27d4f7fc625b',display_name:'DriveBridge — 工作飞书',account_root:true,mountpoint:'/Users/me/DriveBridge/acct',cache_max_bytes:5368709120,cache_max_age:'168h',archived:false,background_pull_excluded_extensions:['.log','.db'],state:{running:true,local_saved:true,queued:3,conflicts:1,cloud_state:'conflict',cache_bytes:1610612736,directory_index:{known:128,cached:96,refreshing:1}}};
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeDiscovery:{create:()=>({setProfiles(){},load:async()=>{}})},addEventListener(){}},fetch:async(route,options)=>{
 const body=options.body?JSON.parse(options.body):null;calls.push({route,method:options.method,body});
 let result={};
 if(route==='/api/config'){
  if(options.method==='POST'){assert.equal(body.action,'mount_policy');mount.background_pull_excluded_extensions=body.background_pull_excluded_extensions;}
  result={platform:'darwin',open_folder_label:'在 Finder 中打开',can_open_folder:true,profiles:[],remotes:[],mounts:[mount]};
 }else if(route==='/api/status')result={can_retry:true,open_folder_label:'在 Finder 中打开',can_open_folder:true,mounts:[mount],conflicts:[]};
 else if(route==='/api/mounts/mount/open'){}
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
 const text=e=>(e.textContent||'')+(e.children||[]).map(text).join('');
 const active=el('mount-rows').children[0],actions=active.children[4].children[0];
 assert.equal(actions.children[0].textContent,'停止同步');
 assert.match(text(active.children[0]),/DriveBridge — 工作飞书/,'friendly name, not the internal account-disk id');
 assert.equal(active.children[0].children[0].children[1].children[0].title,'内部名称：账号盘-c143343f-4a52-4999-b7b0-27d4f7fc625b','raw name stays available as a tooltip');
 assert.match(text(active.children[0]),/正文缓存 1.5 GiB \/ 5 GiB.*7 天未使用后释放/,'cache use against the soft budget and retention');
 assert.match(text(active.children[0]),/目录清单 96 \/ 128.*后台更新中/);
 assert.deepEqual(actions.children.map(c=>c.textContent),['停止同步','在 Finder 中打开','重试同步',undefined],'open uses the host label; retry shown while work is queued');
 await actions.children[1].listeners.click();assert.ok(calls.some(c=>c.route==='/api/mounts/mount/open'&&c.method==='POST'));
 mount={...mount,state:{...mount.state,queued:0,conflicts:0,cloud_state:'synced'}};await context.refresh();
 const quiet=el('mount-rows').children[0].children[4].children[0];
 assert.ok(!quiet.children.some(c=>c.textContent==='重试同步'),'nothing to retry when everything is confirmed');
 mount={...mount,state:{...mount.state,queued:2,cloud_state:'queued'}};await context.refresh();
 const more=el('mount-rows').children[0].children[4].children[0].children.at(-1),panel=more.children[1];
 const remove=panel.children.find(c=>c.textContent==='移除挂载…'),confirm=panel.children.at(-1);
 assert.equal(confirm.hidden,true);await remove.listeners.click();
 assert.ok(!calls.some(c=>c.route.endsWith('/archive')),'removal needs a second, explained click');
 assert.equal(confirm.hidden,false);assert.match(text(confirm),/不会被删除.*可在“已移除挂载”中恢复/);
 await confirm.children[1].children[0].listeners.click();
 assert.equal(el('mount-rows').children.length,0);assert.equal(el('archived-mount-panel').hidden,false);
 const archived=el('archived-mount-rows').children[0];assert.match(archived.children[3].textContent,/队列\s2.*冲突\s0/);
 await archived.children[4].children[0].children[0].listeners.click();
 assert.equal(el('archived-mount-panel').hidden,true);assert.equal(el('mount-rows').children.length,1);
 assert.equal(mount.state.running,false,'restore only restores configuration');
 assert.ok(!calls.some(c=>c.route.endsWith('/start')||c.method==='DELETE'),'archive/restore never starts uploads or deletes cloud content');
 console.log('editable suffix defaults, empty rules, local mount archive and safe restore verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
