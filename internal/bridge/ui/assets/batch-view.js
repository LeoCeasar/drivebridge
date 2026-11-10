'use strict';
((root) => {
  const version = input => ({generation:input.generation,remote_revision:input.remote_revision});
  const sameVersion = (a,b) => a.generation === b.generation && a.remote_revision === b.remote_revision;
  function createQueue() {
    const records = new Map(); let running = false, chain = Promise.resolve();
    const serial = task => { const result = chain.catch(() => {}).then(task); chain = result; return result; };
    function sync(summaries = []) {
      for (const r of records.values()) r.present = false;
      for (const summary of summaries) {
        let r = records.get(summary.id);
        if (!r) { r = {id:summary.id,name:summary.name,selected:false,status:'unreviewed',present:true}; records.set(r.id,r); }
        r.name = summary.name; r.present = true;
      }
      // Preserve edited files and submission outcomes even after they leave the server list.
      for (const [id,r] of records) if (!r.present && !r.input && r.status === 'unreviewed') records.delete(id);
    }
    function load(input, cached) {
      let r = records.get(input.id);
      if (!r?.input) {
        r = {...r,id:input.id,name:input.name,input,text:input.has_draft ? input.Draft : input.Local,seq:0,savedSeq:0,intent:version(input),reviewRequired:Boolean(input.stale || input.blocked || input.has_draft),selected:false,status:'draft',manual:input.has_draft,choices:new Map(),present:true};
        if (cached && typeof cached.text === 'string') {
          r.text = cached.text; r.seq++; r.manual = true;
          r.intent = {generation:cached.generation,remote_revision:cached.revision};
          r.reviewRequired ||= !sameVersion(r.intent,version(input));
        }
        records.set(r.id,r);
      } else {
        const changed = !sameVersion(r.intent,version(input)) || ['Base','Local','Remote'].some(side => r.input[side] !== input[side]);
        r.reviewRequired = r.reviewRequired || changed || input.stale || input.blocked || r.status === 'needs_refresh';
        r.input = input; r.name = input.name;
        if (changed) { r.manual = true; r.choices = new Map(); }
      }
      if (r.reviewRequired) r.status = 'needs_refresh';
      if (!input.text || input.blocked) r.status = 'blocked';
      return r;
    }
    function canSubmit(r) { return Boolean(r?.input?.text && !r.input.stale && !r.input.blocked && !r.reviewRequired && r.status !== 'queued' && r.status !== 'submitting'); }
    function select(id, selected) { const r = records.get(id); if (!r || running || (selected && !canSubmit(r))) return false; r.selected = selected; return true; }
    function edit(id,text) { const r = records.get(id); if (!r?.input?.text) return; r.text = text; r.seq++; if (!['queued','submitting','needs_refresh'].includes(r.status)) r.status = 'draft'; }
    function review(id) { const r = records.get(id); if (!r?.input?.text || r.input.stale || r.input.blocked || running || r.status === 'queued') return false; r.intent = version(r.input); r.reviewRequired = false; r.status = 'draft'; return true; }
    function snapshot(r) { return {id:r.id,text:r.text,seq:r.seq,intent:{...r.intent}}; }
    function saved(r,s) { if (r.seq === s.seq) r.savedSeq = s.seq; }
    async function save(id,io) {
      const r = records.get(id); if (!r?.input?.text || r.status === 'queued') return false;
      const s = snapshot(r);
      await serial(async () => { await io.save(s); saved(r,s); });
      return true;
    }
    async function submit(io,onChange = () => {}, ids) {
      if (running) return false;
      // Freeze every selected draft and its reviewed versions at the user's click.
      const snapshots = [...records.values()].filter(r => (ids ? ids.includes(r.id) : r.selected) && canSubmit(r)).map(snapshot);
      running = true; onChange();
      try {
        for (const s of snapshots) {
          const r = records.get(s.id); r.status = 'submitting'; r.error = ''; onChange(r);
          try {
            await serial(async () => { await io.save(s); saved(r,s); await io.resolve(s); });
            r.status = 'queued'; r.selected = false; r.newerDraft = r.seq !== s.seq;
          } catch (e) {
            const refresh = e.status === 409 || e.code === 'refresh_required';
            r.status = refresh ? 'needs_refresh' : 'failed'; r.error = e.message;
            if (refresh) { r.reviewRequired = true; r.input.stale = true; r.input.refresh_warning = e.status === 409 ? 'version_changed' : 'worker_unavailable'; }
          }
          onChange(r);
        }
      } finally { running = false; onChange(); }
      return true;
    }
    return {records,sync,load,canSubmit,select,edit,review,save,submit,get running() { return running; }};
  }
  const api = {createQueue};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DriveBridgeBatch = api;
})(typeof window === 'undefined' ? globalThis : window);
