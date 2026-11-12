'use strict';
const assert = require('node:assert/strict');
const {createQueue} = require('../assets/batch-view.js');
const input = (id,more = {}) => ({id,name:id + '.md',text:true,stale:false,generation:4,remote_revision:'deleted:token',remote_deleted:true,reason:'remote_deleted',Base:'base',Local:'local edits',Remote:'',Draft:'',has_draft:false,hunks:[],...more});
(async () => {
 const q=createQueue(), text=q.load(input('text')), binary=q.load(input('binary',{text:false}));
 for (const r of [text,binary]) {assert.equal(q.canSubmit(r),false);assert.equal(q.select(r.id,true),false);assert.equal(q.canDecide(r),true);}
 let requests=[];const io={save:async s=>requests.push(['save',s.id,s.text]),resolve:async s=>requests.push(['resolve',s.id,s.action,s.text,s.intent.remote_revision])};
 await q.submit(io);assert.deepEqual(requests,[],'ordinary batch never submits deletion conflicts');
 q.edit(text.id,'edited MD');
 assert.equal(await q.decide(text.id,'restore_local',io),true);
 assert.deepEqual(requests,[['save','text','edited MD'],['resolve','text','restore_local','edited MD','deleted:token']]);
 requests=[];await q.decide(binary.id,'restore_local',io);
 assert.deepEqual(requests,[['resolve','binary','restore_local',undefined,'deleted:token']],'binary restore does not save an empty draft');
 const del=q.load(input('del',{has_draft:true,Draft:'meaningful draft'}));q.review(del.id);
 requests=[];await q.decide(del.id,'accept_delete',io);
 assert.deepEqual(requests,[['resolve','del','accept_delete',undefined,'deleted:token']]);
 assert.equal(q.canDecide(del),false);assert.equal(q.canSubmit(del),false);
 assert.equal(del.decision,'accept_delete');
 assert.equal(del.seq,del.savedSeq,'accepted deletion clears the submitted draft warning without saving empty content');
 const stale=q.load(input('stale',{stale:true}));assert.equal(q.canDecide(stale),false);
 const normal=q.load(input('normal',{remote_deleted:false,reason:'',remote_revision:'r1'}));
 assert.equal(await q.decide(normal.id,'accept_delete',io),false);
 const retained=q.load(input('retained',{remote_deleted:false,reason:'',remote_revision:'r1'}));q.edit(retained.id,'keep edited draft');
 q.sync([{id:'new-partial',name:'new-partial.md'}],true);assert.equal(retained.present,true);assert.equal(q.records.has('new-partial'),true);
 q.sync([]);assert.equal(retained.text,'keep edited draft');assert.equal(q.canSubmit(retained),false);assert.equal(retained.status,'resolved');
 q.sync([{id:retained.id,name:'retained.md'}]);assert.equal(q.canSubmit(retained),false);assert.equal(retained.status,'needs_refresh');
 q.load(input('retained',{remote_revision:'r2'}));assert.equal(retained.text,'keep edited draft');assert.equal(q.canDecide(retained),false);q.review(retained.id);assert.equal(q.canDecide(retained),true);
 console.log('deletion decisions, binary restore, protected drafts, and resolved queue rows verified');
})().catch(e=>{console.error(e);process.exitCode=1;});

(async () => {
 const q=createQueue(), local=q.load(input('local-pending',{remote_deleted:false,local_delete_pending:true,reason:'local_delete_pending',remote_revision:'local:4',text:false}));
 assert.equal(q.canDecide(local),true);assert.equal(q.canSubmit(local),false);assert.equal(q.select(local.id,true),false);
 const calls=[];const io={save:async s=>calls.push(['save',s.id]),resolve:async s=>calls.push(['resolve',s.action,s.text,s.intent.remote_revision])};
 await q.submit(io);assert.deepEqual(calls,[]);
 await q.decide(local.id,'restore_local',io);
 assert.deepEqual(calls,[['resolve','restore_local',undefined,'local:4']]);
 assert.equal(local.decision,'restore_local');
 const changed=q.load(input('changed-local',{remote_deleted:false,local_delete_pending:true,reason:'local_delete_pending',remote_revision:'same'}));q.edit(changed.id,'meaningful edits');
 q.load(input('changed-local',{remote_deleted:true,local_delete_pending:false,reason:'remote_deleted',remote_revision:'same'}));
 assert.equal(changed.text,'meaningful edits');assert.equal(changed.reviewRequired,true);assert.equal(q.canDecide(changed),false);
 console.log('local-only pending deletions require explicit decisions and retain captured binary data');
})().catch(e=>{console.error(e);process.exitCode=1;});
