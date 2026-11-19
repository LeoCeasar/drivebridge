'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
class Element{constructor(){this.children=[];this.listeners={};this.hidden=true;this.value='';this.textContent='';this.classList={toggle(){}};}append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}querySelector(){return {disabled:false};}}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},URLSearchParams};
let source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
source=source.replace('  start();','  globalThis.renderIssues=renderIssues;globalThis.renderGlobalStatus=renderGlobalStatus;');vm.runInNewContext(source,context);
const walk=e=>[e,...e.children.flatMap(walk)],texts=e=>walk(e).map(x=>x.textContent).join(' ');
const waiting={id:'mount',state:{running:true,queued:20,inflight:0,waiting_for_parent:20,cloud_state:'queued',issues:[]}};
context.renderIssues([waiting],true);context.renderGlobalStatus({mounts:[waiting]});
assert.equal(el('sync-issues').hidden,true);assert.equal(el('global-status').textContent,'有内容等待同步');
assert.deepEqual(context.window.DriveBridgeRecovery.cloudChip(waiting.state,true),['等待上级文件夹','pending']);
const unknown={namespace_id:'delete-1',name:'old-test.txt',code:'delete_result_unconfirmed',state:'awaiting_verification',local_saved:true};
context.renderIssues([{id:'mount',state:{running:true,issues:[unknown]}}],true);
assert.match(texts(el('sync-issue-list')),/删除结果待核验/);
assert.match(texts(el('sync-issue-list')),/其他文件.*继续/);
assert.ok(!walk(el('sync-issue-list')).some(e=>e.textContent==='重试同步'),'no blind delete replay button');
assert.ok(!walk(el('sync-issue-list')).some(e=>e.href?.includes('/uploads/')),'namespace operation is not an upload job');
console.log('parent waiting is normal progress; uncertain deletion remains visible without replay');
