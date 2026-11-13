'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const batch = require('../assets/batch-view.js');
class Element {
  constructor() { this.textContent = ''; this.hidden = true; this.value = ''; this.listeners = new Map(); this.classList = {toggle(){}}; this.button = {disabled:false}; this.children = []; this.dataset = {}; }
  addEventListener(kind,listener) { this.listeners.set(kind,listener); }
  replaceChildren(...items) { this.children = items; }
  append(...items) { this.children.push(...items); }
  setAttribute() {}
  querySelector(selector) { return selector === 'button' ? this.button : null; }
}
const elements = new Map();
const element = id => { if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); };
let conflicts = [], detailReads = 0;
let failStatus = true, statusCalls = 0, holdStatus = null, partial = false, mounts = [];
const context = {
  setTimeout,clearTimeout,localStorage:{getItem:()=>null,setItem(){}},
  document:{getElementById:element,querySelectorAll:()=>[],createElement:()=>new Element()},
  window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{makeLineRows:()=>[]},DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:batch,DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},
  location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},URLSearchParams,
  FormData:class { [Symbol.iterator]() { return [][Symbol.iterator](); } },
  fetch:async(route) => {
    if (route === '/api/status') {
      statusCalls++; if (holdStatus) await holdStatus;
      return {ok:!failStatus,status:failStatus ? 503 : 200,json:async()=>failStatus ? {code:'status_unavailable',message:'本机状态读取暂未完成，请稍后刷新。'} : {partial,mounts,conflicts}};
    }
    if (route.startsWith('/api/conflicts/')) { detailReads++; return {ok:true,json:async()=>({id:'a',name:'A',generation:1,remote_revision:'r',revision:'one',source_revision:'s-one',text:true,Base:'base',Local:'local',Remote:'remote',Draft:'',hunks:[]})}; }
    if (route === '/api/config') return {ok:false,status:503,json:async()=>({message:'账号配置操作失败，请重试'})};
    throw new Error('unexpected request');
  }
};
const source = fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
// Exercise the real refresh function without bootstrapping an authenticated browser.
assert.equal(source.split('  start();').length,2);
vm.runInNewContext(source.replace('  start();','  globalThis.refreshStatus = refresh; globalThis.pollDelay = () => statusDelay; globalThis.openConflict = openConflict;'),context);
(async () => {
  failStatus=false; conflicts=[{id:'a',name:'A',revision:'one',source_revision:'s-one'}];
  await context.refreshStatus();
  for(let i=0;i<12;i++) await Promise.resolve();
  assert.equal(detailReads,1,'discovered conflicts must fetch captured inputs before opening');
  await context.openConflict('a');
  assert.equal(detailReads,1,'opening a prefetched conflict must not fetch again');
  assert.equal(element('merged-text').value,'local');
  element('merged-text').value='unsaved';element('merged-text').selectionStart=3;element('merged-text').scrollTop=111;
  await context.refreshStatus();
  assert.equal(detailReads,1);assert.equal(element('merged-text').value,'unsaved');
  assert.equal(element('merged-text').selectionStart,3);assert.equal(element('merged-text').scrollTop,111);
  await context.openConflict('a',true);
  assert.equal(detailReads,2,'explicit refresh must remain available');
  console.log('background prefetch, instant open, explicit refresh and untouched editor verified');
})().catch(error=>{console.error(error);process.exitCode=1;});
