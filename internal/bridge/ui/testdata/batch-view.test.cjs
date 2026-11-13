'use strict';
const assert = require('node:assert/strict');
const {createQueue} = require('../assets/batch-view.js');
const input = (id, more = {}) => ({id,name:id + '.md',text:true,stale:false,generation:4,remote_revision:'r1',Base:'base',Local:'local',Remote:'remote',Draft:'',has_draft:false,hunks:[],...more});
const error = (status,code) => Object.assign(new Error('request failed'),{status,code});
(async () => {
  // Losing per-file records or rebasing retained text silently would lose user intent.
  const q = createQueue();
  const a = q.load(input('a')); q.edit('a','chosen a'); q.select('a',true);
  const b = q.load(input('b')); q.edit('b','chosen b'); q.select('b',true);
  q.load(input('a',{generation:5,remote_revision:'r2',Local:'new local'}));
  assert.equal(a.text,'chosen a');
  assert.equal(a.intent.generation,4);
  assert.equal(a.reviewRequired,true);
  assert.equal(q.canSubmit(a),false);
  q.review('a');
  assert.equal(a.intent.generation,5);
  assert.equal(a.text,'chosen a');
  assert.equal(q.canSubmit(a),true);
  assert.equal(b.text,'chosen b');
  q.load(input('b',{Remote:'changed remote with no revision token'}));
  assert.equal(b.reviewRequired,true);
  assert.equal(b.text,'chosen b');
  const unavailable = q.load(input('stale-first',{stale:true}));
  assert.equal(unavailable.status,'needs_refresh');
  q.load(input('stale-first'));
  assert.equal(q.canSubmit(unavailable),false);
  assert.equal(q.review('stale-first'),true);
  assert.equal(q.canSubmit(unavailable),true);

  // Partial failure must continue independently, retain every draft, and use captured versions.
  const mixed = createQueue();
  for (const id of ['ok','changed','offline','failed']) { mixed.load(input(id)); mixed.edit(id,'merge ' + id); mixed.select(id,true); }
  const events = []; let concurrent = 0, maxConcurrent = 0;
  await mixed.submit({
    save: async s => { events.push(['save',s.id,s.text]); },
    resolve: async s => { concurrent++; maxConcurrent = Math.max(maxConcurrent,concurrent); events.push(['resolve',s.id,s.intent.generation,s.intent.remote_revision,s.text]); await Promise.resolve(); concurrent--; if (s.id === 'changed') throw error(409,'version_changed'); if (s.id === 'offline') throw new TypeError('network disconnected'); if (s.id === 'failed') throw error(503,'refresh_required'); }
  });
  assert.equal(maxConcurrent,3);
  assert.deepEqual([...mixed.records.values()].map(r=>r.status),['queued','needs_refresh','failed','needs_refresh']);
  assert.equal(mixed.records.get('changed').text,'merge changed');
  assert.equal(mixed.records.get('offline').text,'merge offline');
  for (const id of ['ok','changed','offline','failed']) assert.deepEqual(events.filter(e => e[1] === id),[['save',id,'merge '+id],['resolve',id,4,'r1','merge '+id]]);
  assert.equal(events.at(-1)[1],'failed');
  assert.equal(mixed.records.get('ok').selected,false);

  // A draft save failure must never resolve that file or stop the remaining files.
  const saves = createQueue();
  for (const id of ['bad-save','next']) { saves.load(input(id)); saves.select(id,true); }
  const resolved = [];
  await saves.submit({save:async s=>{if(s.id==='bad-save') throw error(503);},resolve:async s=>resolved.push(s.id)});
  assert.deepEqual(resolved,['next']);
  assert.equal(saves.records.get('bad-save').status,'failed');

  // New edits during a request survive success; duplicate clicks send nothing extra.
  const newer = createQueue(); newer.load(input('a')); newer.edit('a','submitted'); newer.select('a',true);
  newer.load(input('b')); newer.edit('b','second submitted'); newer.select('b',true);
  let release; const wait = new Promise(resolve=>{release=resolve;}); let sends = 0;
  const io = {save:async()=>{},resolve:async s=>{sends++; assert.equal(s.text,s.id === 'a' ? 'submitted' : 'second submitted'); await wait;}};
  const pending = newer.submit(io);
  await Promise.resolve(); await Promise.resolve();
  newer.edit('a','newer unsent text'); newer.edit('b','second newer text');
  assert.equal(await newer.submit(io),false);
  release(); await pending;
  assert.equal(sends,2);
  assert.equal(newer.records.get('b').text,'second newer text');
  assert.equal(newer.records.get('b').newerDraft,true);
  assert.equal(newer.records.get('a').text,'newer unsent text');
  assert.equal(newer.records.get('a').newerDraft,true);
  assert.equal(newer.records.get('a').savedSeq < newer.records.get('a').seq,true);

  // Unreviewed, binary, stale and explicitly blocked inputs cannot enter the batch.
  const gated = createQueue(); gated.sync([{id:'unopened',name:'unopened'}]);
  for (const [id,more] of [['binary',{text:false}],['stale',{stale:true}],['blocked',{blocked:true}]]) gated.load(input(id,more));
  for (const r of gated.records.values()) assert.equal(gated.select(r.id,true),false);
  let requests=0; await gated.submit({save:async()=>requests++,resolve:async()=>requests++});
  assert.equal(requests,0);

  // Browser recovery retains its original versions until the user reviews fresh sources.
  const restored = createQueue();
  const r = restored.load(input('cached',{generation:8}),{text:'unsaved cached',generation:4,revision:'r1'});
  assert.equal(r.text,'unsaved cached'); assert.equal(r.reviewRequired,true); assert.equal(r.intent.generation,4);
  assert.equal(restored.canSubmit(r),false);
  const backendDraft = restored.load(input('server-draft',{has_draft:true,Draft:'saved before restart'}));
  assert.equal(backendDraft.text,'saved before restart');
  assert.equal(restored.canSubmit(backendDraft),false);
  assert.equal(restored.review('server-draft'),true);
  assert.equal(restored.canSubmit(backendDraft),true);
  console.log('batch queue preserves per-file intent, partial outcomes, bounded requests, and newer edits');
})().catch(e=>{console.error(e);process.exitCode=1;});
