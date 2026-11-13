'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
class Element {
  constructor(tag = '') { this.tagName=tag; this.value=''; this.textContent=''; this.children=[]; this.listeners={}; this.disabled=false; this.hidden=false; this.attributes={}; this.classList={toggle(){}}; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children=items; }
  addEventListener(kind, fn) { this.listeners[kind]=fn; }
  setAttribute(key,value) { this.attributes[key]=value; }
  async fire(kind) { await this.listeners[kind]?.({preventDefault(){},target:this}); }
}
const elements=new Map(), get=id=>{if(!elements.has(id)) elements.set(id,new Element()); return elements.get(id);};
const document={getElementById:get,createElement:tag=>new Element(tag)};
const initial={path:'/user/conversion-rules.json',revision:'a'.repeat(64),restart_required:false,document:{version:1,rules:[{id:'simple-break-paragraph',source:'<br>',target:'\n',context:'paragraph',match:'simple_html_break'},{id:'simple-break-table',source:'<br>',target:'; ',context:'table',match:'simple_html_break'}]}};
let gets=0, failure=null, saved=null, release;
const api=async(route,method,body)=>{assert.equal(route,'/api/conversion-rules'); if(method!=='PUT'){gets++;return structuredClone(initial);} saved=structuredClone(body); if(release) await new Promise(resolve=>{release=resolve;}); if(failure) throw failure; return {...structuredClone(initial),revision:'b'.repeat(64),document:body.document,restart_required:true};};
const {create}=require('../assets/conversion-rules.js');
let allowDiscard=false, confirmations=0;
const ui=create({document,api,confirm:()=>{confirmations++;return allowDiscard;}});
const rows=()=>get('conversion-rules-rows').children;
const field=(row,index)=>row.children[index].children[0];
(async()=>{
  await ui.load();
  assert.equal(gets,1); assert.equal(field(rows()[0],1).value,'newline');
  assert.equal(field(rows()[0],0).disabled,true); assert.equal(field(rows()[0],3).disabled,true);
  assert.equal(field(rows()[0],4).disabled,true); assert.equal(field(rows()[0],5).disabled,true);
  await get('conversion-rules-save').fire('click');
  assert.equal(saved.document.rules[0].target,'\n','saving untouched defaults must preserve actual LF');
  assert.match(get('conversion-rules-status').textContent,/重启 DriveBridge 后生效；当前同步继续使用现有规则/);
  await get('conversion-rules-add').fire('click');
  const custom=rows()[2], source=field(custom,0), mode=field(custom,1), target=field(custom,2);
  source.value='secret'; await source.fire('input'); mode.value='text'; await mode.fire('change'); target.value='public'; await target.fire('input');
  assert.equal(mode.children.find(x=>x.value==='newline').disabled,true,'literal newline is unsupported');
  await ui.load(); assert.equal(gets,1,'reentering settings must preserve unsaved edits'); assert.equal(rows()[2],custom,'keep focus and controls');
  failure=Object.assign(new Error('规则文件已被修改，草稿已保留'),{status:409,code:'conversion_rules_changed'});
  await get('conversion-rules-save').fire('click');
  const id=saved.document.rules[2].id;
  assert.equal(saved.document.rules[2].target,'public'); assert.equal(rows()[2],custom); assert.equal(source.value,'secret');
  assert.match(get('conversion-rules-status').textContent,/草稿已保留/);
  failure=new Error('target invalid'); await get('conversion-rules-save').fire('click');
  assert.equal(saved.document.rules[2].id,id,'custom IDs remain stable between saves'); assert.equal(rows()[2],custom);
  assert.equal(get('conversion-rules-save').disabled,false);
  await get('conversion-rules-reload').fire('click'); assert.equal(confirmations,1); assert.equal(gets,1); assert.equal(rows()[2],custom,'declined reload retains draft');
  failure=null; await field(custom,5).fire('click'); assert.equal(rows().length,2);
  await get('conversion-rules-save').fire('click'); assert.equal(saved.document.rules.length,2);
  await get('conversion-rules-reload').fire('click'); assert.equal(gets,2,'explicit clean reload reads latest revision without confirmation');
  assert.equal(confirmations,1);
  const app=fs.readFileSync(require.resolve('../assets/app.js'),'utf8');
  const refresh=app.slice(app.indexOf('async function refresh('),app.indexOf('async function refresh(')+7000);
  assert.doesNotMatch(refresh,/conversionRules.*load/,'status polling must not reload rule drafts');
  const html=fs.readFileSync(require.resolve('../assets/index.html'),'utf8');
  assert.match(html,/<section[^>]*id="conversion-rules"/); assert.match(html,/后台自动替换/); assert.match(html,/conversion-rules\.js/);
  console.log('conversion rules preserve newline targets, custom IDs, drafts and focus across load/save errors');
})().catch(e=>{console.error(e);process.exitCode=1;});
