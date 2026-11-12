'use strict';
const assert = require('node:assert/strict');
const {createQueue} = require('../assets/batch-view.js');
const input = (id, more = {}) => ({id,name:id + '.md',text:true,stale:false,generation:4,remote_revision:'r1',Base:'base',Local:'local',Remote:'remote',Draft:'',has_draft:false,hunks:[],...more});
const error = (status,code) => Object.assign(new Error('request failed'),{status,code});
const sleep = ms => new Promise(resolve => setTimeout(resolve,ms));
const gate = () => { let resolve, reject; const promise = new Promise((a,b) => { resolve = a; reject = b; }); return {promise,resolve,reject}; };
const selected = (q,...ids) => { for (const id of ids) { q.load(input(id)); q.edit(id,'merge ' + id); q.select(id,true); } };
const options = {slowMs:20,stallMs:80};

(async () => {
  // Reported case: file A is accepted at once, file B's recheck is slow but succeeds. B is flagged slow,
  // keeps its "submitting" label meanwhile, and ends up accepted without any further user action.
  {
    const q = createQueue(options); selected(q,'a','b');
    const seen = [];
    await q.submit({save:async()=>{},resolve:async s => { if (s.id === 'b') await sleep(50); }},r => { if (r) seen.push([r.id,r.status,r.slow]); });
    assert.deepEqual([...q.records.values()].map(r => r.status),['queued','queued']);
    assert.equal(q.records.get('b').slow,false);
    assert.equal(q.running,false);
    assert.ok(seen.some(([id,status,slow]) => id === 'b' && status === 'submitting' && slow === true),'slow wait must be visible while still submitting');
    assert.ok(!seen.some(([id,status]) => id === 'a' && status !== 'submitting' && status !== 'queued'));
  }

  // A request that never answers cannot hold the controls or the rest of the batch hostage.
  {
    const q = createQueue(options); selected(q,'a','b','c');
    const hung = gate(); const order = []; const changes = [];
    const io = {save:async()=>{},resolve:async s => { order.push(s.id); if (s.id === 'b') await hung.promise; }};
    await q.submit(io,r => { if (r) changes.push([r.id,r.status]); });
    assert.equal(q.running,false,'controls must be released');
    assert.deepEqual(order,['a','b','c'],'the file after the stalled one still ran');
    assert.deepEqual([...q.records.values()].map(r => r.status),['queued','failed','queued']);
    const b = q.records.get('b');
    assert.equal(b.outcomeUnknown,true);
    assert.match(b.error,/本机可能已经接受/);
    assert.equal(b.text,'merge b','draft kept');
    assert.equal(q.canSubmit(b),true,'a stalled file can be retried');
    assert.equal(await q.save('b',{save:async()=>assert.fail('must not write while the old request is pending')}),false);

    // The late answer still lands: success becomes accepted and notifies the page.
    changes.length = 0; hung.resolve();
    await sleep(5);
    assert.equal(b.status,'queued'); assert.equal(b.outcomeUnknown,false); assert.equal(b.error,'');
    assert.deepEqual(changes.at(-1),['b','queued']);
    assert.equal(await q.save('b',{save:async()=>{}}),false,'accepted files are not saved again');
  }

  // A late refusal is reported as a review request, with the draft kept.
  {
    const q = createQueue(options); selected(q,'b');
    const late = gate();
    await q.submit({save:async()=>{},resolve:async()=>late.promise},()=>{});
    assert.equal(q.records.get('b').status,'failed');
    late.reject(error(409,'version_changed')); await sleep(5);
    const b = q.records.get('b');
    assert.equal(b.status,'needs_refresh'); assert.equal(b.reviewRequired,true); assert.equal(b.text,'merge b');
    assert.equal(q.canSubmit(b),false);
  }

  // Acceptance is never downgraded, and an older attempt cannot override a newer retry.
  {
    const q = createQueue(options); selected(q,'b');
    const first = gate();
    await q.submit({save:async()=>{},resolve:async()=>first.promise},()=>{});
    const b = q.records.get('b');
    assert.equal(q.canSubmit(b),true); q.select('b',true);
    await q.submit({save:async()=>{},resolve:async()=>{}},()=>{});
    assert.equal(b.status,'queued');
    first.reject(error(503,'offline')); await sleep(5);
    assert.equal(b.status,'queued','late failure of attempt 1 must not undo the accepted retry');
    assert.equal(b.pending,0);
  }

  // The status poll settles an unknown outcome: gone from the server list means accepted; back again means retry.
  {
    const q = createQueue(options); selected(q,'a','b'); q.sync([{id:'a',name:'a.md'},{id:'b',name:'b.md'}]);
    const hung = gate();
    await q.submit({save:async()=>{},resolve:async s => { if (s.id === 'b') await hung.promise; }},()=>{});
    const b = q.records.get('b');
    q.sync([{id:'b',name:'b.md'}]);
    assert.equal(b.status,'failed','still listed, so still unknown');
    q.sync([]);
    assert.equal(b.status,'queued'); assert.equal(b.inferred,true); assert.equal(b.outcomeUnknown,false);
    q.sync([{id:'b',name:'b.md'}]);
    assert.equal(b.status,'failed'); assert.equal(b.inferred,false); assert.equal(q.canSubmit(b),true);
    hung.resolve(); await sleep(5);
    assert.equal(b.status,'queued');
  }

  // Fast paths keep their timers off the event loop and never flag "slow".
  {
    const q = createQueue({slowMs:1000,stallMs:2000}); selected(q,'a');
    await q.submit({save:async()=>{},resolve:async()=>{}},()=>{});
    assert.equal(q.records.get('a').status,'queued'); assert.equal(q.records.get('a').slow,false);
  }
  console.log('slow and stalled batch items stay visible, release the controls, accept late answers and can be retried');
})().catch(e => { console.error(e); process.exitCode = 1; });
