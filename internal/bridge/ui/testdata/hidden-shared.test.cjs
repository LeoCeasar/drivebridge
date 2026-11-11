'use strict';
const assert = require('node:assert/strict');
const {create} = require('../assets/discovery-view.js');
class Element {
 constructor(tag){this.tag=tag;this.children=[];this.listeners={};this.value='';this.checked=false;this.disabled=false;this.classList={toggle(){}};}
 append(...items){this.children.push(...items);if(this.tag==='select'&&!this.value&&items[0])this.value=items[0].value;}
 replaceChildren(...items){this.children=[];if(this.tag==='select')this.value='';this.append(...items);}
 addEventListener(kind,fn){this.listeners[kind]=fn;}
}
const elements=new Map();const el=id=>{if(!elements.has(id))elements.set(id,new Element(id==='discovery-profile'?'select':'div'));return elements.get(id);};
const document={getElementById:el,createElement:tag=>new Element(tag)};
const profiles=[{id:'a',name:'Account A',authorized:true},{id:'b',name:'Account B',authorized:true},{id:'c',name:'Not configured',authorized:true}];
let views=[{profile_id:'a',configured:true,state:'complete',snapshot:{entries:[{name:'Hidden only',memberships:['shared_hidden']},{name:'Visible too',memberships:['shared_hidden','shared_visible']},{name:'Manual too',memberships:['shared_hidden','manual']}] }},{profile_id:'b',configured:true,state:'complete',sync_hidden_shared:false,snapshot:{entries:[]}},{profile_id:'c',configured:false,state:'unconfigured'}];
views[0].snapshot.entries.push({token:'anchor',name:'Hidden anchor',memberships:['shared_hidden']},{token:'manual-child',name:'Explicit child',manual:true,memberships:['manual'],canonical_root_token:'anchor'});
const calls=[];const posts=new Map();let deferredGet=null;
const api=(route,method='GET',body)=>{
 calls.push({route,method,body});if(method==='GET'){if(deferredGet){const d=deferredGet;deferredGet=null;return d.promise;}return Promise.resolve(structuredClone(views));}
 return new Promise((resolve,reject)=>posts.set(route,{resolve,reject,body}));
};
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
(async()=>{
 const view=create({document,api});view.setProfiles(profiles);await view.load();
 const toggle=el('discovery-hidden-sync');assert.equal(toggle.checked,true,'legacy omitted preference defaults on');assert.equal(toggle.disabled,false);
 const stale=deferred();deferredGet=stale;const oldLoad=view.load();
 toggle.checked=false;const change=toggle.listeners.change();assert.equal(toggle.disabled,true,'pending save disabled');assert.equal(toggle.checked,false,'pending choice remains visible');
 const action=posts.get('/api/discovery/a/hidden-sync');assert.deepEqual(action.body,{enabled:false});
 el('discovery-profile').value='b';el('discovery-profile').listeners.change();assert.equal(toggle.checked,false,'account B keeps its own off state');assert.equal(toggle.disabled,false,'account A save does not disable B');
 toggle.checked=true;const changeB=toggle.listeners.change();assert.equal(toggle.disabled,true);
 views[0].sync_hidden_shared=false;action.resolve(structuredClone(views[0]));await change;
 assert.equal(toggle.checked,true,'late A save cannot reset B pending choice');assert.equal(toggle.disabled,true,'A completion cannot clear B busy state');
 views[1].sync_hidden_shared=true;posts.get('/api/discovery/b/hidden-sync').resolve(structuredClone(views[1]));await changeB;
 stale.resolve([{...views[0],sync_hidden_shared:true}]);await oldLoad;
 assert.equal(el('discovery-profile').value,'b','late A response cannot select A');assert.equal(toggle.checked,true);
 el('discovery-profile').value='a';el('discovery-profile').listeners.change();assert.equal(toggle.checked,false);
 const rows=el('discovery-entries').children;assert.match(rows[0].children[1].textContent,/暂停/);assert.ok(!/暂停/.test(rows[1].children[1].textContent));assert.ok(!/暂停/.test(rows[2].children[1].textContent));assert.ok(!/暂停/.test(rows[3].children[1].textContent),'canonical anchor for explicit child remains available');
 toggle.checked=true;const failed=toggle.listeners.change();posts.get('/api/discovery/a/hidden-sync').reject(new Error('save unavailable'));await failed;assert.equal(toggle.checked,false,'failed save rolls back persisted state');assert.equal(toggle.disabled,false);assert.match(el('discovery-feedback').textContent,/save unavailable/);
 toggle.checked=true;const enable=toggle.listeners.change();views[0].sync_hidden_shared=true;posts.get('/api/discovery/a/hidden-sync').resolve(structuredClone(views[0]));await enable;assert.equal(toggle.checked,true);
 el('discovery-profile').value='c';el('discovery-profile').listeners.change();assert.equal(toggle.disabled,true,'unconfigured account cannot change policy');
 assert.equal(calls.filter(c=>c.method==='POST').length,4);assert.ok(calls.filter(c=>c.method==='POST').every(c=>c.route.endsWith('/hidden-sync')),'toggle cannot login, scan, mount or delete');
 console.log('hidden shared defaults, false saves, account isolation, stale GET protection and failure rollback verified');
})().catch(e=>{console.error(e);process.exitCode=1;});
