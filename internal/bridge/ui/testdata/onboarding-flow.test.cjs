'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const auth = require('../assets/auth-view.js');
class Element {
 constructor(){this.children=[];this.hidden=true;this.listeners={};this.classList={toggle(){}};this.value='';this.button={disabled:false};}
 append(...items){this.children.push(...items);}
 replaceChildren(...items){this.children=items;}
 addEventListener(kind,fn){this.listeners[kind]=fn;}
 querySelector(){return this.button;}
 setAttribute(){}
}
const elements=new Map(),element=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const timers=new Map();let timerID=0,ready=false,beginCalls=0;
const clock={setTimeout(fn){const id=++timerID;timers.set(id,fn);return id;},clearTimeout(id){timers.delete(id);}};
const calls=[];
const context={document:{getElementById:element,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:{create:o=>auth.create({...o,clock})},DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:{},DriveBridgeDiscovery:{create:()=>({setProfiles(){},load:async()=>{},beginLogin:()=>{beginCalls++;}})},addEventListener(){}},location:{},fetch:async(route,options)=>{
 calls.push({route,method:options.method});let body;
 if(route==='/api/config')body={platform:'darwin',profiles:[{id:'profile',name:'Account',brand:'feishu',authorized:ready}],remotes:[],mounts:[]};
 else if(route==='/api/auth/start')body={id:'original',url:'https://accounts.example/consent',auto_complete:true};
 else if(route==='/api/auth/status')body={flow_id:'original',profile_id:'profile',state:ready?'ready':'waiting_authorization',flow:{id:'original',url:'https://accounts.example/consent'},mount_id:ready?'mount':'',mountpoint:ready?'/automatic':''};
 else if(route==='/api/status')body={mounts:[],conflicts:[]};
 else throw new Error('unexpected extra interaction/API: '+route);
 return{ok:true,status:200,json:async()=>body};
}};
const source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
vm.runInNewContext(source.replace('  start();','  globalThis.loadInitialConfig=loadConfig;'),context);
(async()=>{
 await context.loadInitialConfig();
 await element('profiles').children[0].children[1].listeners.click();
 await new Promise(setImmediate);
 assert.equal(element('auth-complete').hidden,true,'normal flow must not require a completion click');
 assert.equal(beginCalls,0,'browser must not start a second login watcher beside the backend');
 ready=true;const [id,fn]=timers.entries().next().value;timers.delete(id);await fn();
 assert.equal(element('auth-flow').hidden,true);assert.match(element('message').textContent,/自动挂载.*无需选择/);
 assert.ok(!calls.some(x=>x.route==='/api/auth/complete'));
 assert.ok(!calls.some(x=>x.method==='POST'&&x.route==='/api/config'),'no manual mount configuration is needed');
 const html=fs.readFileSync(path.join(__dirname,'../assets/index.html'),'utf8');
 assert.match(html,/<details id="advanced-mount-settings"[^>]*>/);
 assert.ok(!/<details id="advanced-mount-settings"[^>]*\bopen\b/.test(html));
 assert.match(html,/<button id="auth-complete"[^>]*hidden/);
 assert.match(html,/src="\/auth-view.js"/);
 console.log('one authorization action automatically reaches mounted UI without confirmation or directory selection');
})().catch(e=>{console.error(e);process.exitCode=1;});
