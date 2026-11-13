'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
class Element {
 constructor() {this.textContent='';this.hidden=true;this.value='';this.disabled=false;this.readOnly=false;this.listeners=new Map();this.classList={toggle(){}};this.children=[];this.dataset={};}
 addEventListener(name,fn){this.listeners.set(name,fn);}
 replaceChildren(...items){this.children=items;}
 append(...items){this.children.push(...items);}
 setAttribute(){}
}
const elements=new Map(), element=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const storage=new Map(), localStorage={getItem:key=>storage.get(key),setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)};
let confirms=0,accept=false;const requests=[],confirmationTexts=[];
const context={
 document:{getElementById:element,querySelectorAll:()=>[],createElement:()=>new Element()},
 window:{DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:require('../assets/diff-view.js'),DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeDiscovery:{create:()=>({})},localStorage,addEventListener(){},confirm:message=>{confirms++;confirmationTexts.push(message);return accept;}},
 localStorage,location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},URLSearchParams,TextEncoder,btoa:s=>Buffer.from(s,'binary').toString('base64'),setTimeout:()=>1,clearTimeout(){},
 fetch:async(route,options)=>{
  requests.push([route,options.method,options.body&&JSON.parse(options.body)]);
  return {ok:true,status:200,json:async()=>route==='/api/status'?{mounts:[],conflicts:[]}:{accepted:true}};
 }
};
const source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
vm.runInNewContext(source.replace('  start();','  globalThis.ui = {queue,showConflict,recoveryControls,renderConflicts};'),context);
const input=(id,more={})=>({id,name:id+'.md',text:true,remote_deleted:true,reason:'remote_deleted',generation:4,remote_revision:'deleted:token',Base:'base',Local:'local',Remote:'',Draft:'',hunks:[],...more});
(async()=>{
 const ui=context.ui,text=ui.queue.load(input('text'));ui.showConflict(text);
 assert.equal(element('remote-deleted').hidden,false);assert.equal(element('resolve').hidden,true);
 assert.equal(element('restore-local').disabled,false);assert.equal(element('accept-delete').disabled,false);
 element('merged-text').value='meaningful edited MD';element('merged-text').listeners.get('input')();
 await element('accept-delete').listeners.get('click')();
 assert.equal(confirms,1);assert.equal(requests.length,0,'canceling the native confirmation sends no save or delete');
 assert.equal(text.text,'meaningful edited MD');assert.match(storage.get('draft:text'),/meaningful edited MD/);
 accept=true;await element('accept-delete').listeners.get('click')();
 assert.equal(confirms,2);assert.equal(requests[0][1],'POST');
 assert.equal(requests[0][2].action,'accept_delete');assert.equal(Object.hasOwn(requests[0][2],'content'),false);
 assert.equal(requests.some(([,method])=>method==='PUT'),false,'follow deletion does not write an empty or pending draft');
 assert.equal(text.seq,text.savedSeq,'accepted delete is no longer an unsaved draft');
 assert.equal(element('accept-delete').disabled,true);assert.equal(element('merged-text').readOnly,true);
 requests.length=0;
 const binary=ui.queue.load(input('binary',{text:false,Base:'',Local:''}));ui.showConflict(binary);
 await element('restore-local').listeners.get('click')();
 assert.equal(confirms,2,'restore does not ask for deletion confirmation');
 assert.equal(requests[0][2].action,'restore_local');assert.equal(Object.hasOwn(requests[0][2],'content'),false);
 assert.equal(requests.some(([,method])=>method==='PUT'),false);
 requests.length=0;
 const local=ui.queue.load(input('local-binary',{text:false,Base:'',Local:'',remote_deleted:false,local_delete_pending:true,reason:'local_delete_pending',remote_revision:'local-delete:local-binary:4'}));ui.showConflict(local);
 assert.equal(element('deletion-title').textContent,'这是尚未上传的本地文件，之前的删除尚未完成。');
 assert.equal(element('restore-local').textContent,'保留本地版本并上传');assert.equal(element('accept-delete').textContent,'确认本地删除（取消上传）');
 assert.match(element('merge-version').textContent,/尚未上传/);assert.doesNotMatch(element('merge-version').textContent,/云端修订|local-delete|deleted:/);
 assert.equal(element('resolve').hidden,true);
 await element('restore-local').listeners.get('click')();
 assert.equal(requests[0][2].action,'restore_local');assert.equal(Object.hasOwn(requests[0][2],'content'),false);
 assert.equal(requests.some(([,method])=>method==='PUT'),false);
 requests.length=0;
 const localDelete=ui.queue.load(input('local-delete',{text:false,Base:'',Local:'',remote_deleted:false,local_delete_pending:true,reason:'local_delete_pending',remote_revision:'local-delete:local-delete:4'}));ui.showConflict(localDelete);
 accept=false;await element('accept-delete').listeners.get('click')();assert.equal(requests.length,0);
 assert.match(confirmationTexts.at(-1),/尚未上传的本地文件并取消上传/);assert.doesNotMatch(confirmationTexts.at(-1),/跟随云端/);
 accept=true;await element('accept-delete').listeners.get('click')();
 assert.equal(requests[0][2].action,'accept_delete');assert.equal(Object.hasOwn(requests[0][2],'content'),false);
 assert.equal(element('draft-state').textContent,'已确认本地删除，上传已取消');assert.equal(element('message').textContent,'已确认本地删除，上传已取消。');
 ui.showConflict(localDelete);assert.equal(element('draft-state').textContent,'已确认本地删除，上传已取消');
 ui.showConflict(text);assert.equal(element('draft-state').textContent,'已跟随云端删除');assert.equal(element('deletion-title').textContent,'云端文件已删除或已移出同步目录');
 const missing=ui.queue.load(input('missing'));ui.showConflict(missing);ui.renderConflicts([]);ui.recoveryControls();
 assert.equal(element('conflict-count').textContent,0);assert.equal(element('restore-local').disabled,true);assert.equal(element('accept-delete').disabled,true);
 // A different pending file must not disable this file's review and submit controls.
 const busy=ui.queue.load(input('busy',{remote_deleted:false,reason:'overlapping_edits'}));
 let releaseBusy;
 const busySubmission=ui.queue.submit({save:async()=>{},resolve:()=>new Promise(resolve=>{releaseBusy=resolve;})},()=>{},['busy']);
 await new Promise(setImmediate);
 assert.equal(ui.queue.running,true);assert.equal(busy.pending,1);
 const eligible=ui.queue.load(input('eligible',{remote_deleted:false,reason:'overlapping_edits',can_export_copies:true}));
 ui.showConflict(eligible);
 assert.equal(element('resolve').disabled,false,'another pending file must not disable submit');
 assert.equal(element('save-draft').disabled,false,'another pending file must not disable saving');
 assert.equal(element('refresh-conflict').disabled,false,'another pending file must not disable refresh');
 assert.match(element('merge-version').textContent,/双方.*相同段落/);
 ui.queue.select(eligible.id,true);ui.renderConflicts();
 assert.equal(element('submit-selected').disabled,false,'eligible selection can submit during a pending request');
 busy.status='failed';busy.outcomeUnknown=true;ui.showConflict(busy);
 assert.equal(element('resolve').disabled,true,'detached pending request cannot be resubmitted');
 assert.equal(element('save-draft').disabled,true,'late draft write cannot race a new save');
 assert.equal(element('refresh-conflict').disabled,true,'pending captured sources cannot be refreshed');
 releaseBusy();await busySubmission;
 console.log('deletion banner, native confirmation, binary restore request and inactive resolved controls verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
