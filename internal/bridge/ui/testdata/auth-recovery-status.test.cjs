'use strict';
// Regression: a protected authorization wait must explain itself beside its mount,
// including after opening another tab, without erasing real per-file issues.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
class Element {
 constructor(){this.children=[];this.listeners={};this.hidden=true;this.disabled=false;this.value='';this.textContent='';this.classList={toggle(){}};}
 append(...x){this.children.push(...x);}replaceChildren(...x){this.children=x;}addEventListener(k,f){this.listeners[k]=f;}setAttribute(){}querySelector(){return {disabled:false};}scrollIntoView(){}
}
const elements=new Map(),el=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const text=e=>[e.textContent,...e.children.map(text)].join(' '),walk=e=>[e,...e.children.flatMap(walk)];
const context={document:{getElementById:el,createElement:()=>new Element(),querySelectorAll:()=>[]},window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeDiscovery:{create:()=>({setProfiles(){},load:async()=>{}})},addEventListener(){}},location:{},URLSearchParams};
let source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
vm.runInNewContext(source.replace('  start();','  globalThis.renderMounts=renderMounts;globalThis.renderIssues=renderIssues;globalThis.renderGlobalStatus=renderGlobalStatus;globalThis.loadConfig=loadConfig;'),context);
const progress={flow_id:'flow',profile_id:'profile',state:'waiting_authorization',started_at:'2000-01-01T00:00:00Z',flow:{id:'flow',url:'https://accounts.feishu.cn/consent',expires_at:'2099-01-01T00:00:00Z'}};
const mount={id:'mount',name:'账号盘',profile_id:'profile',status_warning:'pending',status_updated_at:'2026-11-17T02:46:42Z',state:{running:false,last_error:'identity_changed',cloud_state:'synced',queued:2,local_saved:true},authorization:progress};
context.renderMounts([mount,{id:'other',name:'另一账号',profile_id:'other',state:{running:false}}]);
assert.match(text(el('mount-rows')),/等待飞书授权确认/);
let actions=walk(el('mount-rows'));
assert.ok(actions.some(e=>e.textContent==='等待授权'&&e.disabled),'mount remains protected with an explanatory action');
assert.ok(actions.some(e=>e.textContent==='继续授权'&&e.href===progress.flow.url),'new tab receives the current link from backend status');
assert.ok(actions.some(e=>e.textContent==='启动挂载'&&!e.disabled),'other profiles remain independent');
assert.match(text(el('mount-rows')),/授权.*按钮/,'long waits explain the last consent step');
assert.ok(!/账号身份需要重新核对/.test(text(el('mount-rows'))),'old identity error must not dominate active progress');
context.renderIssues([mount],true);assert.equal(el('sync-issues').hidden,true,'mount-level historical error does not become a fake upload task');
context.renderIssues([{...mount,state:{...mount.state,issues:[{job_id:'job',code:'unsupported_content',name:'真实失败.md',local_saved:true}]}}],true);
assert.match(text(el('sync-issue-list')),/真实失败.md/,'real per-file failures survive authorization');
context.renderGlobalStatus({partial:true,mounts:[mount]});assert.match(el('global-status').textContent,/等待.*授权/);
for(const [state,label] of [['opening_login','核验'],['waiting_login','登录'],['mounting','挂载']]){
 context.renderMounts([{...mount,authorization:{...progress,state}}]);assert.match(text(el('mount-rows')),new RegExp(label));
 assert.ok(!walk(el('mount-rows')).some(e=>e.textContent==='继续授权'),'only a consent wait exposes its URL');
}
context.renderMounts([{...mount,authorization:{...progress,state:'failed',error:'expired'}}]);
assert.match(text(el('mount-rows')),/过期|超时/);assert.ok(walk(el('mount-rows')).some(e=>e.textContent==='重新授权'));
assert.ok(!walk(el('mount-rows')).some(e=>e.textContent==='继续授权'),'expired link cannot remain actionable');
context.renderMounts([{...mount,authorization:{...progress,flow:{...progress.flow,expires_at:'2001-01-01T00:00:00Z'}}}]);
assert.ok(!walk(el('mount-rows')).some(e=>e.textContent==='继续授权'),'expired active flows cannot offer a stale consent link');
assert.match(text(el('mount-rows')),/过期/);
context.renderMounts([{...mount,status_warning:'',state:{running:true,cloud_state:'synced'},authorization:{...progress,state:'ready'}}]);
assert.ok(walk(el('mount-rows')).some(e=>e.textContent==='暂停同步'&&!e.disabled));assert.ok(!/等待飞书授权确认/.test(text(el('mount-rows'))));
context.renderMounts([{...mount,status_warning:'',state:{running:true,cloud_state:'blocked',last_error:'unsupported_content'},authorization:{...progress,state:'failed',error:'expired'}}]);
assert.ok(!/授权或登录等待已超时/.test(text(el('mount-rows'))),'a healthy restarted worker supersedes a past workflow failure, even if a file later fails');
assert.match(text(el('mount-rows')),/无法自动同步/);
// An auth failure gets an actionable explanation, not a file editing-permission hint.
context.renderIssues([{id:'missing',name:'账号盘',state:{last_error:'unauthorized'}}]);
assert.match(text(el('sync-issue-list')),/重新授权/);assert.ok(!/所有者恢复编辑权限/.test(text(el('sync-issue-list'))));
context.renderIssues([{id:'storage',name:'账号盘',state:{last_error:'credential_unavailable'}}]);assert.match(text(el('sync-issue-list')),/钥匙串|凭据/);
(async()=>{
 context.fetch=async()=>({ok:true,json:async()=>({profiles:[{id:'profile',name:'工作账号',brand:'feishu',authorized:true}],remotes:[],mounts:[]})});
 await context.loadConfig();assert.match(text(el('profiles')),/账号已绑定/);assert.ok(!/飞书，已授权/.test(text(el('profiles'))));
 console.log('authorization-owned mount wait, safe continuation, expiry, account isolation and historical-error handling verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
