'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const batch = require('../assets/batch-view.js');
class Element {
  constructor() { this.textContent = ''; this.hidden = true; this.value = ''; this.listeners = new Map(); this.classList = {toggle(){}}; this.button = {disabled:false}; }
  addEventListener(kind,listener) { this.listeners.set(kind,listener); }
  replaceChildren() {}
  append() {}
  querySelector() { return this.button; }
}
const elements = new Map();
const element = id => { if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); };
let failStatus = true;
const context = {
  document:{getElementById:element,querySelectorAll:()=>[],createElement:()=>new Element()},
  window:{DriveBridgeDiff:{},DriveBridgeRecovery:{},DriveBridgeBatch:batch,DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},
  location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},URLSearchParams,
  FormData:class { [Symbol.iterator]() { return [][Symbol.iterator](); } },
  fetch:async(route) => {
    if (route === '/api/session') return new Promise(()=>{}); // Keep bootstrap independent from this test.
    if (route === '/api/status') return {ok:!failStatus,status:failStatus ? 503 : 200,json:async()=>failStatus ? {code:'offline',message:'状态读取暂不可用'} : {mounts:[],conflicts:[]}};
    if (route === '/api/config') return {ok:false,status:503,json:async()=>({message:'账号配置操作失败，请重试'})};
    throw new Error('unexpected request');
  }
};
vm.runInNewContext(fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8'),context);
(async () => {
  const refresh = element('refresh').listeners.get('click');
  await refresh();
  assert.equal(element('message').textContent,'状态读取暂不可用');
  assert.equal(element('message').hidden,false);
  failStatus = false; await refresh();
  assert.equal(element('connection').textContent,'已连接本机服务');
  assert.equal(element('message').hidden,true,'successful refresh clears its own stale error');
  failStatus = true; await refresh();
  const form = element('profile-form');
  await form.listeners.get('submit')({preventDefault(){},currentTarget:form});
  assert.equal(element('message').textContent,'账号配置操作失败，请重试');
  failStatus = false; await refresh();
  assert.equal(element('message').hidden,false,'refresh must preserve a newer user-action notice');
  assert.equal(element('message').textContent,'账号配置操作失败，请重试');
  console.log('status recovery clears only its own error and preserves user-action feedback');
})().catch(error=>{console.error(error);process.exitCode=1;});
