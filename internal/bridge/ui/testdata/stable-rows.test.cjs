'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const batch = require('../assets/batch-view.js');
class Element {
  constructor() { this.textContent = ''; this.hidden = true; this.value = ''; this.listeners = new Map(); this.classList = {toggle(){}}; this.button = {disabled:false}; this.children = []; }
  addEventListener(kind,listener) { this.listeners.set(kind,listener); }
  replaceChildren(...items) { this.children = items; for(const item of items) item.parent=this; }
  insertBefore(item,next) { if(item.parent) item.remove();const index=next ? this.children.indexOf(next) : this.children.length;this.children.splice(index,0,item);item.parent=this; }
  remove() { this.removals=(this.removals || 0)+1; if(this.parent) {const index=this.parent.children.indexOf(this);if(index>=0)this.parent.children.splice(index,1);this.parent=null;} }
  contains(item) { return item === this || this.children.some(child => child.contains(item)); }
  append(...items) { this.children.push(...items); for(const item of items)item.parent=this; }
  setAttribute() {}
  querySelector(selector) { return selector === 'button' ? this.button : null; }
}
const elements = new Map();
const element = id => { if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); };
let failStatus = true, statusCalls = 0, holdStatus = null, partial = false, mounts = [];
const context = {
  document:{getElementById:element,querySelectorAll:()=>[],createElement:()=>new Element()},
  window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:batch,DriveBridgeDiscovery:{create:()=>({})},addEventListener(){}},
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
vm.runInNewContext(source.replace('  start();','  globalThis.refreshStatus = refresh; globalThis.pollDelay = () => statusDelay;'),context);
(async () => {
  failStatus = false;
  const one={id:'b',name:'B',state:{running:true,cloud_state:'synced'},status_updated_at:'one'};
  const two={id:'a',name:'A',state:{running:true,cloud_state:'synced'},status_updated_at:'one'};
  mounts=[one,two];await context.refreshStatus();
  const firstRows=[...element('mount-rows').children];
  mounts=[{...two,status_updated_at:'two'},{...one,status_updated_at:'two'}];await context.refreshStatus();
  assert.ok(element('mount-rows').children.every((row,index) => row === firstRows[index]),'rescans and observation timestamps must preserve row identity and order');
  element('merged-text').value='my unsaved draft';element('merged-text').selectionStart=3;element('merged-text').scrollTop=110;
  mounts=[{...two,state:{...two.state,queued:2}},one];await context.refreshStatus();
  assert.equal(element('mount-rows').children[1],firstRows[1],'a counter change must retain other rows');
  assert.equal(element('merged-text').value,'my unsaved draft');
  assert.equal(element('merged-text').selectionStart,3);assert.equal(element('merged-text').scrollTop,110);
  const focusedRow=element('mount-rows').children[0];
  context.document.activeElement=focusedRow.children[4];
  mounts=[{...two,state:{...two.state,queued:4}},one];await context.refreshStatus();
  assert.equal(element('mount-rows').children[0],focusedRow,'a focused row must remain attached during changed status');
  const later=element('mount-rows').children[1], beforeRemovals=later.removals || 0;
  context.document.activeElement=later.children[4];await context.refreshStatus();
  assert.equal(later.removals || 0,beforeRemovals,'replacing an earlier row must not detach a focused later row');
  context.document.activeElement=null;await context.refreshStatus();
  assert.notEqual(element('mount-rows').children[0],focusedRow,'deferred status changes must apply after focus leaves');
  console.log('stable keyed mount rows and untouched editor verified');
})().catch(error=>{console.error(error);process.exitCode=1;});
