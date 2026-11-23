'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
class Element{constructor(){this.children=[];this.listeners={};this.value='';this.hidden=false;this.classList={toggle(){}};}append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const calls=[];let release;
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeAuth:{create:()=>({})},DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},fetch:async(route,options)=>{calls.push({route,body:JSON.parse(options.body)});await new Promise(resolve=>{release=resolve});return{ok:true,json:async()=>({accepted:true})};}};
const source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
vm.runInNewContext(source.replace('  start();','  globalThis.menu=moreMenu;globalThis.meta=mountMeta;globalThis.enable=()=>{canRefreshDirectories=true;};'),context);
const text=e=>(e.textContent||'')+(e.children||[]).map(text).join('');
(async()=>{
 context.enable();
 const mount={id:'m',state:{running:true,cloud_state:'synced'}};
 const panel=context.menu(mount,false).children[1];
 const form=panel.children.find(e=>e.listeners.submit);
 assert.ok(form,'missing directory refresh form');
 const input=form.children[0].children.find(e=>e.type==='text'),button=form.children.find(e=>e.type==='submit');
 input.value='个人目录/开发项目文档';const first=form.listeners.submit({preventDefault(){}});
 assert.equal(button.disabled,true);await form.listeners.submit({preventDefault(){}});
 assert.equal(calls.length,1,'inflight duplicate clicks merged');assert.deepEqual(calls[0],{route:'/api/mounts/m/refresh-directory',body:{remote:'个人目录/开发项目文档'}});
 release();await first;assert.match(el('message').textContent,/已请求.*清单.*后台/);
 const paused=context.menu({...mount,state:{running:false}},false).children[1].children.find(e=>e.listeners.submit);
 assert.equal(paused.children.find(e=>e.type==='submit').disabled,true);
 const stale=context.menu(mount,true).children[1].children.find(e=>e.listeners.submit);
 assert.equal(stale.children.find(e=>e.type==='submit').disabled,true);
 const meta=context.meta({}, {cloud_sync_interval:'5m',directory_index:{known:1864,cached:1864,stale:1850,pending:1,failed:1,last_refreshed:'2026-11-23T10:30:00Z',recent:[{remote:'个人目录/开发项目文档',state:'waiting',cached_at:'2026-11-23T08:53:06Z',next_attempt_at:'2026-11-23T10:31:00Z'}]}});
 assert.match(text(meta),/目录缓存覆盖.*1864.*1864/);assert.match(text(meta),/较旧 1850.*待刷新 1/);assert.match(text(meta),/保留旧清单/);assert.match(text(meta),/个人目录\/开发项目文档.*等待/);
 assert.deepEqual(context.window.DriveBridgeRecovery.cloudChip(mount.state,true),['上传已确认','ok']);
 console.log('scoped directory requests, duplicate suppression, stopped/stale controls and cache-age status verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
