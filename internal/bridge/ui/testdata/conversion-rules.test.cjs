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
let gets=0, puts=0, failure=null, saved=null, restartRequired=true, holdLoad=null;
const api=async(route,method,body)=>{assert.equal(route,'/api/conversion-rules'); if(method!=='PUT'){gets++; if(holdLoad) await holdLoad; return structuredClone(initial);} puts++; saved=structuredClone(body); if(failure) throw failure; return {...structuredClone(initial),revision:'b'.repeat(64),document:body.document,restart_required:restartRequired};};
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
  restartRequired=false; await get('conversion-rules-save').fire('click');
  assert.match(get('conversion-rules-status').textContent,/已保存，当前规则已生效/);
  assert.doesNotMatch(get('conversion-rules-status').textContent,/重启/);
  restartRequired=true;
  await get('conversion-rules-add').fire('click');
  const custom=rows()[2], source=field(custom,0), mode=field(custom,1), target=field(custom,2);
  assert.equal(mode.value,'text','new rules start in text mode, so filling the source never silently deletes it');
  assert.equal(target.disabled,false,'new rule target is immediately editable');
  source.value='secret'; await source.fire('input');
  const beforeEmpty=puts; await get('conversion-rules-save').fire('click');
  assert.equal(puts,beforeEmpty,'empty text targets require text or an explicit delete choice');
  assert.match(get('conversion-rules-status').textContent,/填写.*文本|选择.*删除/);
  mode.value='delete'; await mode.fire('change'); failure=new Error('preserve draft');
  await get('conversion-rules-save').fire('click'); assert.equal(puts,beforeEmpty+1); assert.equal(saved.document.rules[2].target,'');
  failure=null; mode.value='text'; await mode.fire('change'); target.value='public'; await target.fire('input');
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
  const retained=rows()[0]; let rejectReload;
  holdLoad=new Promise((resolve,reject)=>{rejectReload=reject;});
  const loading=get('conversion-rules-reload').fire('click');
  assert.equal(get('conversion-rules-add').disabled,true,'reload locks new rows while awaiting disk');
  assert.equal(get('conversion-rules-save').disabled,true,'reload locks saves while awaiting disk');
  assert.equal(field(retained,1).disabled,true,'reload locks editable controls');
  const previousPuts=puts;
  await get('conversion-rules-save').fire('click'); await get('conversion-rules-add').fire('click');
  assert.equal(puts,previousPuts,'save handler also rejects reload races');
  assert.equal(rows().length,2);
  rejectReload(new Error('disk unavailable')); await loading; holdLoad=null;
  assert.equal(rows()[0],retained,'reload failure preserves existing draft and controls');
  assert.equal(field(retained,1).disabled,false); assert.equal(get('conversion-rules-save').disabled,false);
  await ui.load(); assert.equal(gets,3,'navigation after reload failure preserves retained draft');
  const app=fs.readFileSync(require.resolve('../assets/app.js'),'utf8');
  const refresh=app.slice(app.indexOf('async function refresh('),app.indexOf('async function refresh(')+7000);
  assert.doesNotMatch(refresh,/conversionRules.*load/,'status polling must not reload rule drafts');
  const html=fs.readFileSync(require.resolve('../assets/index.html'),'utf8');
  assert.match(html,/<section[^>]*id="conversion-rules"/); assert.match(html,/后台自动替换/); assert.match(html,/conversion-rules\.js/);
  initial.document.rules.push({id:'yaml-front-matter',source:'---',target:'yaml_code_block',context:'document',match:'front_matter'});
  initial.document.rules.push({id:'plain-table-resize',source:'table',target:'preserve_cells',context:'document',match:'table_resize'});
  await get('conversion-rules-reload').fire('click');
  const yamlRow=rows()[2];
  for(let i=0;i<6;i++) assert.equal(field(yamlRow,i).disabled,true,'YAML structural controls are fixed');
  const tableRow=rows()[3];
  for(let i=0;i<6;i++) assert.equal(field(tableRow,i).disabled,true,'table structural controls are fixed');
  assert.equal(field(tableRow,1).value,'preserve_cells');
  assert.equal(field(yamlRow,1).value,'yaml_code_block');
  assert.match(field(yamlRow,2).value,/无损保留/);
  await get('conversion-rules-save').fire('click');
  assert.deepEqual(saved.document.rules.slice(2),initial.document.rules.slice(2),'saving preserves both complete structural rules');
  initial.document.rules.push(
    {id:'basic-inline-html',source:'strong|b|em|i',target:'native_emphasis',context:'document',match:'basic_inline_html'},
    {id:'simple-html-paragraph',source:'p|div',target:'native_paragraph',context:'document',match:'simple_html_paragraph'},
    {id:'indented-code',source:'indented_code',target:'native_code_block',context:'document',match:'indented_code'},
    {id:'ordinary-dollar-text',source:'$',target:'preserve_text',context:'document',match:'ordinary_dollar_text'}
  );
  await get('conversion-rules-reload').fire('click');
  for(const row of rows().slice(4)) {
    for(let i=0;i<6;i++) assert.equal(field(row,i).disabled,true,'safe defaults are fixed policies');
    assert.doesNotMatch(field(row,2).value,/native_|preserve_text/,'policy targets have friendly labels');
    assert.equal(field(row,1).children.length,1,'policies have no deletion target');
  }
  assert.match(field(rows()[4],0).value,/<strong>/);
  assert.match(field(rows()[5],2).value,/段落/);
  assert.match(field(rows()[6],2).value,/代码/);
  assert.match(field(rows()[7],2).value,/保留/);
  await get('conversion-rules-save').fire('click');
  assert.deepEqual(saved.document,initial.document,'friendly labels preserve structured JSON');
  console.log('conversion rules preserve newline targets, custom IDs, drafts and focus across load/save errors');
})().catch(e=>{console.error(e);process.exitCode=1;});
