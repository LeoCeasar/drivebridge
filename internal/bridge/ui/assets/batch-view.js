'use strict';
((root) => {
  const version = input => ({generation:input.generation,remote_revision:input.remote_revision});
  const sameVersion = (a,b) => a.generation === b.generation && a.remote_revision === b.remote_revision;
  // A cloud recheck normally takes a few seconds, but every Feishu request of one account shares a
  // single lock, so a busy account can make it much slower. slowMs only flags the wait; stallMs stops
  // blocking the rest of the batch and the controls, while the request keeps running and its late answer
  // is still applied. No file may stay in "submitting" forever.
  const deletionDecision = input => Boolean(input?.remote_deleted || input?.local_delete_pending);
  const defaults = {slowMs:10000,stallMs:90000};
  function createQueue(options = {}) {
    const {slowMs,stallMs} = {...defaults,...options};
    const records = new Map(); let running = false, chain = Promise.resolve();
    // A request that gave up waiting (detach) must not hold back later saves and submissions.
    const serial = (task, detach) => { const result = chain.catch(() => {}).then(task); chain = (detach ? Promise.race([result,detach]) : result).catch(() => {}); return result; };
    function sync(summaries = [], partial = false) {
      if (!partial) for (const r of records.values()) r.present = false;
      for (const summary of [...summaries].sort((a,b) => (a.name || a.id).localeCompare(b.name || b.id) || a.id.localeCompare(b.id))) {
        let r = records.get(summary.id);
        if (!r) { r = {id:summary.id,name:summary.name,selected:false,status:'unreviewed',present:true}; records.set(r.id,r); }
        if (r.status === 'resolved') { r.status = r.input ? 'needs_refresh' : 'unreviewed'; r.reviewRequired = Boolean(r.input); }
        r.name = summary.name; r.present = true;
        r.revision = summary.revision; r.sourceRevision = summary.source_revision;
        if (r.input?.source_revision && summary.source_revision) {
          r.sourcesChanged = r.input.source_revision !== summary.source_revision;
          if (r.sourcesChanged && !['queued','submitting'].includes(r.status)) { r.reviewRequired = true; r.status = 'needs_refresh'; }
        }
        // The list showed it again, so the earlier inferred acceptance was wrong.
        if (r.inferred && r.status === 'queued') { r.status = 'failed'; r.inferred = false; r.selected = false; r.error = '云端列表中仍有此文件，上次提交没有被确认，草稿已保留，可重试。'; }
      }
      // A request that outlived its wait and whose conflict has left the server list was accepted.
      for (const r of records.values()) if (!r.present && r.outcomeUnknown) { r.status = 'queued'; r.inferred = true; r.outcomeUnknown = false; r.slow = false; r.selected = false; r.error = ''; r.newerDraft = r.seq !== r.attemptSeq; }
      // Preserve edited files and submission outcomes even after they leave the server list.
      for (const [id,r] of records) if (!r.present) {
        r.selected = false;
        if (!r.input && r.status === 'unreviewed') records.delete(id);
        else if (!['queued','submitting'].includes(r.status)) r.status = 'resolved';
      }
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
        const changed = !sameVersion(r.intent,version(input)) || r.input.remote_deleted !== input.remote_deleted || r.input.local_delete_pending !== input.local_delete_pending || ['Base','Local','Remote'].some(side => r.input[side] !== input[side]);
        r.reviewRequired = r.reviewRequired || changed || input.stale || input.blocked || r.status === 'needs_refresh';
        r.input = input; r.name = input.name;
        if (changed) { r.manual = true; r.choices = new Map(); }
      }
      r.sourcesChanged = Boolean(r.sourceRevision && input.source_revision !== r.sourceRevision);
      if (r.reviewRequired) r.status = 'needs_refresh';
      if ((!input.text && !deletionDecision(input)) || input.blocked) r.status = 'blocked';
      if (deletionDecision(input)) r.selected = false;
      return r;
    }
    function actionable(r) { return Boolean(r?.present && r.input && !r.input.stale && !r.input.blocked && !r.sourcesChanged && !r.reviewRequired && !['queued','submitting','resolved'].includes(r.status)); }
    function canSubmit(r) { return Boolean(actionable(r) && r.input.text && !deletionDecision(r.input)); }
    function canDecide(r) { return Boolean(actionable(r) && deletionDecision(r.input)); }
    function select(id, selected) { const r = records.get(id); if (!r || running || (selected && !canSubmit(r))) return false; r.selected = selected; return true; }
    function edit(id,text) { const r = records.get(id); if (!r?.input?.text || r.status === 'resolved') return; r.text = text; r.seq++; if (!['queued','submitting','needs_refresh'].includes(r.status)) r.status = 'draft'; }
    function review(id) { const r = records.get(id); if (!r?.present || r.sourcesChanged || (!r.input?.text && !deletionDecision(r.input)) || r.input.stale || r.input.blocked || running || ['queued','resolved'].includes(r.status)) return false; r.intent = version(r.input); r.reviewRequired = false; r.status = 'draft'; return true; }
    function snapshot(r) { return {id:r.id,text:r.text,seq:r.seq,intent:{...r.intent}}; }
    function saved(r,s) { if (r.seq === s.seq) r.savedSeq = s.seq; }
    async function save(id,io) {
      // An abandoned request may still be about to save this file's draft; a second writer could be overwritten by it.
      const r = records.get(id); if (!r?.present || !r.input?.text || ['queued','resolved'].includes(r.status) || r.pending > 0) return false;
      const s = snapshot(r);
      await serial(async () => { await io.save(s); saved(r,s); });
      return true;
    }
    // Applies one request's final answer. Acceptance is never downgraded, and a stale answer from an
    // earlier attempt cannot overwrite the outcome of a newer one.
    function settle(r,s,attempt,error) {
      r.pending = Math.max(0,(r.pending || 0) - 1);
      if (!error) {
        r.status = 'queued'; r.selected = false; r.newerDraft = r.seq !== s.seq; r.error = ''; r.slow = false; r.outcomeUnknown = false; r.inferred = false;
        return;
      }
      if (r.status === 'queued' || r.attempt !== attempt) return;
      const refresh = error.status === 409 || error.code === 'refresh_required';
      r.status = refresh ? 'needs_refresh' : 'failed'; r.error = error.message; r.slow = false; r.outcomeUnknown = false;
      if (refresh) { r.reviewRequired = true; r.input.stale = true; r.input.refresh_warning = error.status === 409 ? 'version_changed' : 'worker_unavailable'; }
    }
    async function attempt(r,s,io,onChange) {
      const number = r.attempt = (r.attempt || 0) + 1;
      r.pending = (r.pending || 0) + 1; r.attemptSeq = s.seq;
      r.status = 'submitting'; r.error = ''; r.slow = false; r.outcomeUnknown = false; r.inferred = false; onChange(r);
      let open = true, giveUp; const detach = new Promise(resolve => { giveUp = resolve; });
      const slowTimer = setTimeout(() => { if (open && r.attempt === number) { r.slow = true; onChange(r); } },slowMs);
      const stallTimer = setTimeout(() => {
        if (!open || r.attempt !== number) return;
        r.status = 'failed'; r.slow = false; r.outcomeUnknown = true;
        r.error = `等待超过 ${Math.round(stallMs / 1000)} 秒仍没有结果。草稿已保留，本机可能已经接受；刷新后若列表中已没有此文件即已接受，否则可重试。`;
        onChange(r); giveUp();
      },stallMs);
      const done = serial(async () => { await io.save(s); saved(r,s); await io.resolve(s); },detach).then(
        () => { open = false; clearTimeout(slowTimer); clearTimeout(stallTimer); settle(r,s,number,null); onChange(r); },
        error => { open = false; clearTimeout(slowTimer); clearTimeout(stallTimer); settle(r,s,number,error); onChange(r); });
      await Promise.race([done,detach]);
    }
    async function submit(io,onChange = () => {}, ids) {
      if (running) return false;
      // Freeze every selected draft and its reviewed versions at the user's click.
      const snapshots = [...records.values()].filter(r => (ids ? ids.includes(r.id) : r.selected) && canSubmit(r)).map(snapshot);
      running = true; onChange();
      try { for (const s of snapshots) await attempt(records.get(s.id),s,io,onChange); }
      finally { running = false; onChange(); }
      return true;
    }
    async function decide(id,action,io,onChange = () => {}) {
      const r = records.get(id);
      if (running || !canDecide(r) || !['restore_local','accept_delete'].includes(action)) return false;
      const s = {...snapshot(r),action,text:action === 'restore_local' && r.input.text ? r.text : undefined};
      running = true; r.status = 'submitting'; r.error = ''; onChange(r);
      try {
        await serial(async () => { if (s.text !== undefined) { await io.save(s); saved(r,s); } await io.resolve(s); });
        if (action === 'accept_delete') saved(r,s);
        r.decision = action;
        r.status = 'queued'; r.selected = false; r.newerDraft = r.seq !== s.seq;
      } catch (e) {
        const refresh = e.status === 409 || e.code === 'refresh_required';
        r.status = refresh ? 'needs_refresh' : 'failed'; r.error = e.message;
        if (refresh) { r.reviewRequired = true; r.input.stale = true; r.input.refresh_warning = e.status === 409 ? 'version_changed' : 'worker_unavailable'; }
      } finally { running = false; onChange(r); }
      return true;
    }
    return {records,sync,load,canSubmit,canDecide,select,edit,review,save,submit,decide,get running() { return running; }};
  }
  const api = {createQueue};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DriveBridgeBatch = api;
})(typeof window === 'undefined' ? globalThis : window);
