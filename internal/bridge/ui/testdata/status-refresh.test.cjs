'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const batch = require('../assets/batch-view.js');
class Element {
  constructor() { this.textContent = ''; this.hidden = true; this.value = ''; this.listeners = new Map(); this.classList = {toggle(){}}; this.button = {disabled:false}; this.children = []; }
  addEventListener(kind,listener) { this.listeners.set(kind,listener); }
  replaceChildren(...items) { this.children = items; }
  append(...items) { this.children.push(...items); }
  setAttribute() {}
  querySelector() { return this.button; }
}
const elements = new Map();
const element = id => { if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); };
let failStatus = true, statusCalls = 0, holdStatus = null, partial = false, mounts = [], rulesLoads = 0;
const context = {
  document:{getElementById:element,querySelectorAll:()=>[],createElement:()=>new Element()},
  window:{DriveBridgeRules:{create:()=>({load:()=>{rulesLoads++;}})},DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:batch,DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},
  location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},URLSearchParams,
  FormData:class { [Symbol.iterator]() { return [][Symbol.iterator](); } },
  fetch:async(route) => {
    if (route === '/api/status') {
      statusCalls++; if (holdStatus) await holdStatus;
      return {ok:!failStatus,status:failStatus ? 503 : 200,json:async()=>failStatus ? {code:'status_unavailable',message:'本机状态读取暂未完成，请稍后刷新。'} : {partial,mounts,conflicts:[]}};
    }
    if (route === '/api/config') return {ok:false,status:503,json:async()=>({message:'账号配置操作失败，请重试'})};
    throw new Error('unexpected request');
  }
};
const source = fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
// Exercise the real refresh function without bootstrapping an authenticated browser.
assert.equal(source.split('  start();').length,2);
vm.runInNewContext(source.replace('  start();','  globalThis.refreshStatus = refresh; globalThis.pollDelay = () => statusDelay; globalThis.showView = view;'),context);
(async () => {
  context.showView('settings');
  assert.equal(rulesLoads,1,'entering settings loads conversion rules');
  await context.refreshStatus();
  assert.equal(element('message').hidden,true,'automatic polling must not replace the page with a cloud/draft error banner');
  assert.match(element('connection').textContent,/状态.*稍后|暂未|更新/);
  assert.equal(context.pollDelay(),10000);
  const refresh = element('refresh').listeners.get('click');
  await refresh();
  assert.equal(element('message').textContent,'本机状态读取暂未完成，请稍后刷新。');
  assert.equal(context.pollDelay(),20000);
  failStatus = false; await context.refreshStatus();
  assert.equal(element('connection').textContent,'已连接本机服务');
  assert.equal(context.pollDelay(),5000);
  assert.equal(element('message').hidden,true,'successful refresh clears only its own stale error');
  const form = element('profile-form');
  await form.listeners.get('submit')({preventDefault(){},currentTarget:form});
  failStatus = true; await context.refreshStatus();
  assert.equal(element('message').textContent,'账号配置操作失败，请重试','automatic failure must not overwrite a user-action notice');
  failStatus = false; await context.refreshStatus();
  assert.equal(element('message').hidden,false,'refresh must preserve a newer user-action notice');
  let release; holdStatus = new Promise(resolve => { release = resolve; });
  const before = statusCalls;
  const first = context.refreshStatus(),second = context.refreshStatus();
  assert.equal(statusCalls,before+1,'slow requests must not overlap across polls/clicks');
  release(); await Promise.all([first,second]); holdStatus = null;
  partial = true;
  mounts = [{id:'mount',name:'cached mount',status_warning:'pending',status_updated_at:'2026-11-11T08:00:00Z',state:{running:true,local_saved:true,cloud_state:'synced',queued:3}}];
  await context.refreshStatus();
  const row = element('mount-rows').children[0];
  assert.equal(row.children[2].children[0].textContent,'上次：已确认');
  assert.equal(row.children[4].children[0].children[0].disabled,true,'stale state must not drive start/stop actions');
  assert.match(element('connection').textContent,/部分状态/);
  assert.equal(element('conflict-count').textContent,'待更新','incomplete observations cannot promise zero conflicts');
  assert.equal(element('message').textContent,'账号配置操作失败，请重试');
  assert.equal(rulesLoads,1,'manual refresh and status polling never reload the conversion rules editor');
  console.log('quiet polling, partial status, single-flight and user-action feedback verified');
})().catch(error=>{console.error(error);process.exitCode=1;});
