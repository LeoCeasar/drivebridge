'use strict';
const assert = require('node:assert/strict');
const {createQueue} = require('../assets/batch-view.js');
const input = id => ({id,name:id,text:true,generation:4,remote_revision:'reviewed',Local:'local',Remote:'remote',Base:'base'});
const gate = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return {promise,resolve}; };
const tick = () => new Promise(r => setImmediate(r));
(async () => {
  // A pending cloud read must not hold another file behind it until the stall timer.
  const q = createQueue({slowMs:1000,stallMs:2000});
  for (const id of ['slow','healthy','later']) { q.load(input(id)); q.edit(id,'merge '+id); }
  q.select('slow',true); q.select('healthy',true);
  const held = gate(); let writes = 0;
  const io = {save:async () => { writes++; },resolve:async s => { assert.deepEqual(s.intent,{generation:4,remote_revision:'reviewed'}); if (s.id === 'slow') await held.promise; }};
  const first = q.submit(io);
  try {
    await tick();
    assert.equal(q.records.get('healthy').status,'queued','healthy file must finish while another request is still pending');
    assert.equal(q.select('later',true),true,'other files remain selectable');
    assert.equal(await q.submit(io,()=>{},['slow']),false,'same-file duplicate is refused');
    const later = q.submit(io,()=>{},['later']); await later;
    assert.equal(q.records.get('later').status,'queued','new eligible files can submit during another request');
    assert.equal(writes,3);
  } finally { held.resolve(); await first; }

  // Bound cloud work even when more files are selected, without serializing the batch.
  const bounded = createQueue({slowMs:1000,stallMs:2000}); const release = gate();
  for (let i=0;i<7;i++) { bounded.load(input(String(i))); if (i < 4) bounded.select(String(i),true); }
  let active=0,max=0,started=0;
  const boundedIO = {save:async()=>{},resolve:async()=>{ active++; started++; max=Math.max(max,active); await release.promise; active--; }};
  const run = bounded.submit(boundedIO); let overlap;
  try {
    await tick(); assert.equal(started,3,'three independent requests should start');
    overlap = bounded.submit(boundedIO,()=>{},['4','5','6']); await tick();
    assert.equal(started,3,'overlapping batches share the same request limit');
  } finally { release.resolve(); await Promise.all([run,overlap]); }
  assert.equal(max,3); assert.equal(started,7);

  // Saves for one file remain ordered, while another file can save independently.
  const drafts = createQueue(); for (const id of ['a','b']) drafts.load(input(id));
  const old = gate(), stored = new Map();
  const saveIO = {save:async s => { if (s.id === 'a' && s.text === 'older') await old.promise; stored.set(s.id,s.text); }};
  drafts.edit('a','older'); const older = drafts.save('a',saveIO); await tick();
  drafts.edit('a','newer'); const newer = drafts.save('a',saveIO);
  drafts.edit('b','independent'); const independent = drafts.save('b',saveIO);
  try { await tick(); assert.equal(stored.get('b'),'independent'); }
  finally { old.resolve(); await Promise.all([older,newer,independent]); }
  assert.equal(stored.get('a'),'newer','late older save must not overwrite the latest draft');
  // A timed-out draft write still owns the file: a late old write cannot replace a newer save.
  const timed = createQueue({slowMs:5,stallMs:15}); timed.load(input('timed')); timed.edit('timed','older'); timed.select('timed',true);
  const oldWrite = gate(); let disk;
  const timedIO = {save:async s => { await oldWrite.promise; disk=s.text; },resolve:async()=>{throw new Error('offline');}};
  await timed.submit(timedIO); timed.edit('timed','newer');
  assert.equal(await timed.save('timed',{save:async()=>assert.fail('late writer still owns this file')}),false);
  assert.equal(await timed.submit(timedIO,()=>{},['timed']),false);
  oldWrite.resolve(); await tick();
  assert.equal(disk,'older'); assert.equal(timed.records.get('timed').text,'newer');
  assert.equal(await timed.save('timed',{save:async s=>{disk=s.text;}}),true);
  assert.equal(disk,'newer');
  console.log('batch files run independently with bounded work, same-file ordering and reviewed versions');
})().catch(e => { console.error(e); process.exitCode=1; });
