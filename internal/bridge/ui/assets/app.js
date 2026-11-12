'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const diff = window.DriveBridgeDiff;
  const recovery = window.DriveBridgeRecovery;
  const queue = window.DriveBridgeBatch.createQueue();
  let activeHunk = null, openSeq = 0, activeView = 'workspace';
  let csrf = '', current = null, choices = new Map(), manual = false;
  let draftTimer = null, authFlow = '', authProfile = '', authAutomatic = false, authConfigFlow = '';
  let messageOwner = '', statusFlight = null, statusDelay = 5000, statusPartial = false;
  const record = () => current && queue.records.get(current.id);
  const make = (tag, text, className) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (className) e.className = className; return e; };
  function message(text, error = false, owner = '') { messageOwner = owner; $('message').textContent = text; $('message').classList.toggle('error', error); $('message').hidden = !text; }
  function showSessionError() { $('session-error').hidden = false; $('workspace').hidden = true; $('settings').hidden = true; $('connection').textContent = '本机会话已失效'; }
  async function api(route, method = 'GET', body) {
    const headers = {};
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
    const response = await fetch(route, {method, headers, credentials: 'same-origin', cache: 'no-store', body: body === undefined ? undefined : JSON.stringify(body)});
    let data = {}; try { data = await response.json(); } catch (_) { /* Stable fallback below. */ }
    if (!response.ok) { if (response.status === 403) showSessionError(); const error = new Error(data.message || '本机服务暂时不可用，请稍后重试。'); error.status = response.status; error.code = data.code; throw error; }
    return data;
  }
  const discovery = window.DriveBridgeDiscovery.create({document,api,onChanged:() => refresh()});
  const authMonitor = window.DriveBridgeAuth.create({api,storage:window.localStorage,onProgress:renderAuthProgress,onError:(error,terminal) => {
    $('auth-flow').hidden = false;
    $('auth-state').textContent = terminal ? '上次授权进度会话已结束；已保存账号和挂载状态以当前页面为准。' : '进度连接暂未更新；后台会继续等待授权，稍后自动重试。';
    $('auth-complete').hidden = terminal;
  }});
  function showAuthFlow(flow,profileID) {
    authFlow = flow.id; authProfile = profileID; authAutomatic = flow.auto_complete === true; authConfigFlow = '';
    $('auth-flow').hidden = false; $('auth-link').hidden = false; $('auth-link').href = flow.url;
    $('auth-complete').hidden = authAutomatic;
    $('auth-state').textContent = authAutomatic ? '请在飞书页面完成授权，后台将自动继续，无需返回确认。' : '请在飞书页面授权后检查结果。';
    if (authAutomatic) authMonitor.track(flow,profileID);
  }
  async function renderAuthProgress(progress) {
    authFlow = progress.flow_id; authProfile = progress.profile_id; authAutomatic = true;
    $('auth-flow').hidden = progress.state === 'ready'; $('auth-complete').hidden = true;
    $('auth-link').hidden = progress.state !== 'waiting_authorization';
    if (progress.flow?.url) $('auth-link').href = progress.flow.url;
    const labels = {waiting_authorization:'请在飞书页面完成授权，后台会自动继续。',opening_login:'授权已验证，正在自动打开应用登录窗口…',waiting_login:'请在应用窗口登录飞书；登录后自动挂载，无需选择目录。',ready:'账号盘已自动挂载。'};
    $('auth-state').textContent = labels[progress.state] || '正在接续登录…';
    if (progress.state === 'failed') {
      const errors = {mount_archived:'账号盘已移除，请先在“已移除挂载”中恢复配置。',expired:'授权或登录等待已超时，可以重新发起登录。',canceled:'本次登录已取消。',partial:'目录入口尚未完整读取，可重新扫描后检查挂载。',needs_runtime:'请安装 Microsoft Edge WebView2 Runtime，再从账号盘区域重新打开登录窗口。',unsupported:'应用内登录组件不可用，请检查系统版本和完整安装包。',needs_browser:'应用登录组件不可用，请检查是否安装完整应用。',needs_login:'网页登录尚未完成，可从账号盘区域重新打开登录窗口。',unauthorized:'用户授权尚未完成，请重新发起授权。',identity_changed:'授权身份与已有账号不一致，请核对账号。',driver_unavailable:'挂载驱动不可用，请检查系统驱动。'};
      const reason = errors[progress.error] || '自动登录或挂载尚未完成，请查看账号盘状态后重试。';
      $('auth-state').textContent = reason + ' 已保存的授权和同步数据会保留。';
      message($('auth-state').textContent,true);
    }
    if (progress.state !== 'waiting_authorization' && (authConfigFlow !== progress.flow_id || ['ready','failed'].includes(progress.state))) {
      authConfigFlow = progress.flow_id; await loadConfig(); await refresh();
    }
    if (progress.state === 'ready' && authFlow === progress.flow_id) {authFlow = ''; authProfile = ''; message('登录完成，账号盘已自动挂载，无需选择本机目录。');}
  }
  function bytes(text) { const data = new TextEncoder().encode(text); let result = ''; for (let i = 0; i < data.length; i += 8192) result += String.fromCharCode(...data.subarray(i, i + 8192)); return btoa(result); }
  function view(name) {
    activeView = name;
    for (const key of ['workspace', 'settings']) $(key).hidden = key !== (name === 'conflicts' ? 'workspace' : name);
    document.querySelectorAll('[data-view]').forEach(e => e.classList.toggle('active', e.dataset.view === name));
    const conflicts = name === 'conflicts';
    $('page-label').textContent = name === 'settings' ? '账号与目录' : conflicts ? '冲突与合并' : '文件与同步';
    $('workspace-title').textContent = conflicts ? '冲突与合并' : '文件与同步';
    $('workspace-description').textContent = conflicts ? '核对每份文件的差异，保留草稿并提交已选结果。' : '查看本机保存和云端确认，处理需要你决定的差异。';
    $('mount-panel').hidden = conflicts; $('setup-shortcut').hidden = conflicts;
  }
  document.querySelectorAll('[data-view]').forEach(e => e.addEventListener('click', () => view(e.dataset.view)));
  for (const id of ['setup-shortcut', 'start-setup']) $(id).addEventListener('click', () => view('settings'));
  function cloudLabel(state) { const value = state.cloud_state; const labels = {synced:'已确认',queued:'等待同步',uploading:'正在上传',merging:'正在合并',awaiting_verification:'等待回读核验',conflict:'需要合并',blocked:'已阻塞',paused:'已暂停',offline:'离线'}; return labels[value] || (state.running ? '等待确认' : '挂载未启动'); }
  function renderMounts(mounts = [], canRetry = false) {
    $('mount-rows').replaceChildren(); $('archived-mount-rows').replaceChildren();
    $('mount-empty').hidden = mounts.some(m => !m.archived);
    $('archived-mount-panel').hidden = !mounts.some(m => m.archived);
    for (const mount of mounts) {
      const stale = Boolean(mount.status_warning), known = !stale || Boolean(mount.status_updated_at);
      const state = mount.state || {}, row = make('tr'); const name = make('td'); name.append(make('span', mount.name, 'mount-name'), make('span', mount.mountpoint, 'mount-path'));
      if (state.directory_index) {
        const index = state.directory_index;
        name.append(make('small',`目录清单：已缓存 ${index.cached || 0} / 已知 ${index.known || 0} 个目录${index.refreshing ? '，后台更新中' : ''}${index.failed ? '；部分更新失败，保留旧清单' : ''}`,'mount-path'));
      }
      if (stale) {
        const reasons = {pending:'本机正在处理其他状态查询',auth_required:'账号需要重新授权',identity_changed:'账号身份需要重新核对',worker_unavailable:'挂载服务暂不可用'};
        const reason = reasons[mount.status_warning] || '本机状态更新暂未完成';
        name.append(make('small',known ? `${reason}；保留上次结果（${new Date(mount.status_updated_at).toLocaleTimeString()}）` : `${reason}；尚未取得完整状态`,'mount-path'));
      }
      const local = make('td'); local.append(make('span', !known ? '等待核对' : (stale ? '上次：' : '') + (state.local_saved ? '已持久保存' : (state.running ? '检查中或仍有本机写入' : '挂载未启动')), 'state-tag' + (state.local_saved ? '' : ' pending')));
      const cloud = make('td', !known ? '等待核对' : (stale ? '上次：' : '') + cloudLabel(state)); const queue = make('td',!known ? '暂不可读' : `${stale ? '上次：' : ''}队列 ${state.queued || 0}，在途 ${state.inflight || 0}，冲突 ${state.conflicts || 0}`); const action = make('td'); const button = make('button', mount.archived ? '恢复挂载配置' : state.running ? '停止同步' : '启动挂载', 'button small');
      button.disabled = stale;
      button.addEventListener('click', async () => { button.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/${mount.archived ? 'restore' : state.running ? 'stop' : 'start'}`, 'POST', {}); if (mount.archived) message('挂载配置已恢复；需要你再启动同步，保留数据尚未上传。'); await loadConfig(); await refresh(); } catch (e) { message(e.message, true); } finally { button.disabled = false; } }); action.append(button);
      if (!mount.archived) {
        const remove = make('button','移除挂载','button small'); remove.disabled = stale; remove.title = '停止并从常规列表移除；保留飞书文件及本机恢复数据';
        remove.addEventListener('click',async () => { remove.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/archive`,'POST',{}); message('挂载已移除；本机数据保留，可在下方“已移除挂载”中恢复。'); await loadConfig(); await refresh(); } catch(error) { message(error.message,true); } finally { remove.disabled = stale; } }); action.append(remove);
      }
      if (canRetry && !mount.archived) { const retry = make('button','重试同步','button small'); retry.disabled = stale || !recovery.canRetry(mount,canRetry); retry.title = state.running ? '重新核验并重试待同步项目' : '请先启动挂载'; retry.addEventListener('click',async () => { retry.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/retry`,'POST',{}); message('已请求重新核验并重试同步，请查看云端状态。'); await refresh(); } catch(e) { message(e.message,true); } finally { retry.disabled = stale || !recovery.canRetry(mount,canRetry); } }); action.append(retry); }
      row.append(name, local, cloud, queue, action); $(mount.archived ? 'archived-mount-rows' : 'mount-rows').append(row);
    }
  }
  const outcomeLabels = {unreviewed:'请先打开复核',draft:'待提交',submitting:'正在提交',queued:'已接受，等待同步',resolved:'已处理，草稿保留',needs_refresh:'需要刷新并复核',failed:'提交失败，可重试',blocked:'无法自动合并，请保存副本'};
  function renderQueue() {
    const records = [...queue.records.values()];
    $('queue-rows').replaceChildren(); $('queue-empty').hidden = records.length > 0;
    for (const r of records) {
      const row = make('tr'), select = make('td'), checkbox = make('input'); checkbox.type = 'checkbox'; checkbox.id = `queue-select-${r.id}`; checkbox.checked = r.selected; checkbox.disabled = queue.running || (!r.selected && !queue.canSubmit(r)); checkbox.setAttribute('aria-label',`选择 ${r.name || '待合并文件'}`);
      checkbox.addEventListener('change',() => { queue.select(r.id,checkbox.checked); renderQueue(); $(checkbox.id)?.focus(); }); select.append(checkbox);
      const file = make('td'), open = make('button',r.name || '待合并文件','text-button queue-file'); open.disabled = r.status === 'resolved'; open.addEventListener('click',() => openConflict(r.id)); file.append(open);
      const draft = make('td',!r.input ? '未打开' : !r.input.text ? '非文本，保留副本' : r.seq !== r.savedSeq ? '页面草稿已保留' : r.savedSeq || r.input.has_draft ? '本机草稿已保存' : '来源已载入，尚未修改');
      const result = make('td'); result.append(make('span',r.status === 'queued' && r.decision === 'accept_delete' ? '已跟随云端删除' : outcomeLabels[r.status],'queue-outcome ' + r.status));
      if (r.input?.remote_deleted && !['queued','resolved'].includes(r.status)) result.append(make('small','云端已删除，请打开文件单独决定。','queue-detail'));
      if (r.status === 'failed') result.append(make('small',r.error || '请检查本机连接后重试。','queue-detail'));
      if (r.status === 'needs_refresh') result.append(make('small','草稿已保留；打开后刷新差异，再核对来源。','queue-detail'));
      if (r.newerDraft) result.append(make('small','本次提交后的新编辑仍保留在页面中，尚未提交。请复制保存。','queue-detail'));
      row.append(select,file,draft,result); $('queue-rows').append(row);
    }
    const count = status => records.filter(r => r.status === status).length;
    const selected = records.filter(r => r.selected && queue.canSubmit(r)).length;
    $('submit-selected').textContent = queue.running ? '正在逐个提交…' : `提交已选文件（${selected}）`;
    $('submit-selected').disabled = queue.running || !selected;
    $('select-reviewed').disabled = queue.running || !records.some(r => queue.canSubmit(r)); $('clear-selection').disabled = queue.running || !records.some(r => r.selected);
    $('queue-summary').textContent = `共 ${records.length} 份，已选 ${selected} 份；本机已接受 ${count('queued')} 份，需要复核 ${count('needs_refresh')} 份，失败 ${count('failed')} 份。`;
  }
  function renderConflicts(conflicts, partial = false) {
    if (conflicts) queue.sync(conflicts,partial);
    const records = [...queue.records.values()];
    const count = records.filter(r => r.present && r.status !== 'queued').length;
    $('conflict-count').textContent = statusPartial ? (count ? `${count}（待更新）` : '待更新') : count; $('conflict-list').replaceChildren(); $('conflict-empty').hidden = records.length > 0;
    for (const r of records) { const button = make('button',r.name || '待合并文件','conflict-item' + (current?.id === r.id ? ' selected' : '')); button.disabled = r.status === 'resolved'; button.title = r.name || ''; button.append(make('small',outcomeLabels[r.status])); button.addEventListener('click',() => openConflict(r.id)); $('conflict-list').append(button); }
    renderQueue();
  }
  function refresh(interactive = false) {
    if (statusFlight) return statusFlight;
    statusFlight = (async () => {
      try {
        const status = await api('/api/status');
        statusPartial = Boolean(status.partial);
        renderMounts(status.mounts,status.can_retry); renderConflicts(status.conflicts,status.partial); if (current) recoveryControls();
        statusDelay = 5000;
        $('connection').textContent = status.partial ? '已连接本机服务，部分状态稍后更新' : '已连接本机服务';
        if (messageOwner === 'status_refresh') message('');
      } catch (e) {
        statusDelay = Math.min(30000,statusDelay * 2);
        if (e.status === 403) return;
        $('connection').textContent = '本机状态暂未更新，将自动重试';
        // Background observation failures must not overwrite an editing/action notice.
        if (interactive) message('本机状态读取暂未完成，请稍后刷新。',true,'status_refresh');
      }
    })().finally(() => { statusFlight = null; });
    return statusFlight;
  }
  function scheduleStatusPoll() {
    setTimeout(async () => {
      try { await refresh(); if (activeView === 'settings') await discovery.load(); }
      finally { scheduleStatusPoll(); }
    },statusDelay);
  }
  $('refresh').addEventListener('click',() => refresh(true));
  function options(select, items, label) { select.replaceChildren(); if (!items.length) { const empty = make('option', label); empty.value = ''; select.append(empty); } for (const item of items) { const option = make('option', item.name); option.value = item.id; select.append(option); } }
  let policyMounts = [];
  function renderPullPolicy() {
    const selected = policyMounts.find(m => m.id === $('pull-policy-mount').value) || policyMounts[0];
    $('pull-policy-fields').disabled = !selected;
    $('pull-policy-extensions').value = (selected?.background_pull_excluded_extensions || []).join(', ');
  }
  $('pull-policy-mount').addEventListener('change',renderPullPolicy);
  $('pull-policy-form').addEventListener('submit',async event => {
    event.preventDefault(); $('pull-policy-fields').disabled = true;
    const values = $('pull-policy-extensions').value.split(/[\s,;]+/).filter(Boolean);
    try { await api('/api/config','POST',{action:'mount_policy',mount_id:$('pull-policy-mount').value,background_pull_excluded_extensions:values}); message('过滤规则已保存；后台后续检查将使用新规则，显式读取和本地上传保持可用。'); await loadConfig(); }
    catch(error) { message(error.message,true); } finally { $('pull-policy-fields').disabled = !policyMounts.length; }
  });
  async function loadConfig() {
    const config = await api('/api/config'); $('macos-backend-field').hidden = config.platform !== 'darwin'; options($('profile-select'), config.profiles.filter(p => p.authorized), '先完成账号授权'); options($('remote-select'), config.remotes, '先添加共享目录'); $('profiles').replaceChildren();
    for (const p of config.profiles) { const row = make('div', undefined, 'profile-row'); const title = make('div', p.name); title.append(make('small', `${p.brand === 'lark' ? 'Lark' : '飞书'}，${p.authorized ? '已授权' : '待授权'}`)); const button = make('button', p.authorized ? '重新授权' : '授权', 'button small'); button.addEventListener('click', async () => { button.disabled = true; try { const flow = await api('/api/auth/start', 'POST', {profile_id:p.id}); showAuthFlow(flow,p.id); } catch (e) { message(e.message, true); } finally { button.disabled = false; } }); row.append(title, button); $('profiles').append(row); if (p.authorized && config.can_recover) { const recovery = make('button', '停止相关挂载并重新授权，保留待上传数据', 'text-button recovery-auth'); recovery.addEventListener('click', async () => { recovery.disabled = true; try { const flow = await api('/api/auth/start','POST',{profile_id:p.id,recovery:true}); showAuthFlow(flow,p.id); } catch (e) { message(e.message,true); } finally { recovery.disabled = false; } }); $('profiles').append(recovery); } }
    const previousPolicyMount = $('pull-policy-mount').value; policyMounts = (config.mounts || []).filter(m => !m.archived);
    options($('pull-policy-mount'),policyMounts,'暂无挂载');
    if (policyMounts.some(m => m.id === previousPolicyMount)) $('pull-policy-mount').value = previousPolicyMount; else $('pull-policy-mount').value = policyMounts[0]?.id || '';
    renderPullPolicy();
    discovery.setProfiles(config.profiles); await discovery.load();
    return config;
  }
  for (const [id, action] of [['profile-form','profile'],['remote-form','remote'],['mount-form','mount']]) $(id).addEventListener('submit', async event => { event.preventDefault(); const form = event.currentTarget; const button = form.querySelector('button'); button.disabled = true; const data = Object.fromEntries(new FormData(form)); data.action = action; try { await api('/api/config', 'POST', data); message(action === 'mount' ? '挂载配置已保存，可在“文件与同步”中启动。' : '配置已保存。'); form.reset(); await loadConfig(); await refresh(); } catch (e) { message(e.message, true); } finally { button.disabled = false; } });
  $('auth-complete').addEventListener('click', async () => { if (authAutomatic) { await authMonitor.checkNow(); return; } if (!authFlow) return; $('auth-complete').disabled = true; $('auth-state').textContent = '正在等待授权结果并核验身份…'; try { await api('/api/auth/complete','POST',{flow_id:authFlow}); $('auth-flow').hidden = true; authFlow = ''; const authorizedProfile = authProfile; authProfile = ''; message('用户授权成功，正在打开账号盘的应用登录窗口。'); await loadConfig(); if (authorizedProfile) { const started = await discovery.beginLogin(authorizedProfile); if (!started) message('用户授权成功。请在“账号盘与自动目录发现”中检查账号状态，再打开应用登录窗口继续设置。'); } } catch (e) { $('auth-state').textContent = e.message; message(e.message, true); } finally { $('auth-complete').disabled = false; } });
  function stash(r = record()) {
    if (!r?.input?.text) return;
    try { localStorage.setItem(`draft:${r.id}`,JSON.stringify({text:r.text,generation:r.intent.generation,revision:r.intent.remote_revision})); } catch (_) { /* Keep the in-page and backend copies. */ }
  }
  function draftState(text) { $('draft-state').textContent = text; }
  function recoveryControls() {
    const r = record(), state = recovery.conflictState(current,queue.running);
    const complete = ['queued','resolved'].includes(r?.status);
    $('merged-text').disabled = !state.canEdit; $('merged-text').readOnly = complete || (current?.remote_deleted && queue.running);
    $('save-draft').disabled = !state.canSaveDraft || complete || queue.running;
    $('resolve').hidden = Boolean(current?.remote_deleted);
    $('resolve').disabled = queue.running || !queue.canSubmit(r);
    $('resolve').title = r?.reviewRequired || current?.stale ? '请先刷新差异并重新核对来源' : '';
    $('reset-choices').hidden = Boolean(current?.remote_deleted);
    $('reset-choices').disabled = !state.canEdit || queue.running || complete;
    $('refresh-conflict').disabled = queue.running || complete;
    $('remote-deleted').hidden = !current?.remote_deleted;
    $('restore-local').disabled = $('accept-delete').disabled = queue.running || !queue.canDecide(r);
    $('review-sources').hidden = !r?.reviewRequired || current?.stale || (!current?.text && !current?.remote_deleted) || complete;
    $('review-sources').disabled = queue.running;
    $('later').disabled = queue.running;
    $('export-copies').hidden = !current?.can_export_copies;
    $('export-copies').disabled = !state.canExport;
    const warning = state.warning || (r?.reviewRequired ? '草稿保持原样。请对照当前来源核对已恢复的草稿，再点击“已核对新来源”允许提交。' : '');
    $('source-warning').textContent = warning; $('source-warning').hidden = !warning;
  }
  const draftIO = {save:s => api(`/api/conflicts/${encodeURIComponent(s.id)}/draft`,'PUT',{content:bytes(s.text)}),resolve:s => api(`/api/conflicts/${encodeURIComponent(s.id)}/resolve`,'POST',{conflict_id:s.id,expected_generation:s.intent.generation,expected_remote_revision:s.intent.remote_revision,...(s.action ? {action:s.action} : {}),...(s.text === undefined ? {} : {content:bytes(s.text)})})};
  async function saveDraft(id = current?.id) {
    const r = queue.records.get(id); if (!r?.input?.text || ['queued','resolved'].includes(r.status)) return true;
    if (queue.running) return false;
    clearTimeout(draftTimer); stash(r);
    try { await queue.save(id,draftIO); if (current?.id === id) draftState(r.seq === r.savedSeq ? '草稿已保存到本机' : '新编辑已保留，等待保存'); renderQueue(); return true; }
    catch(e) { if (current?.id === id) { draftState('页面草稿已保留，等待恢复连接'); message(e.message + ' 编辑仍保留在此页面，请恢复连接后保存。',true); } renderQueue(); return false; }
  }
  function scheduleSave() { const id = current?.id; clearTimeout(draftTimer); draftTimer = setTimeout(() => { if (!queue.running) saveDraft(id); },650); }
  function edited() { const r = record(); if (!r) return; queue.edit(r.id,$('merged-text').value); r.manual = manual = true; stash(); draftState('正在编辑，稍后自动保存'); document.querySelectorAll('.choice').forEach(e => e.disabled = true); scheduleSave(); renderQueue(); }
  $('merged-text').addEventListener('input',edited); $('save-draft').addEventListener('click',() => saveDraft());
  function rebuild() {
    if (!current) return;
    $('merged-text').value = diff.mergeChoices(current.Base,current.hunks,choices);
    queue.edit(current.id,$('merged-text').value); stash(); draftState('差异选择已更新，稍后自动保存'); scheduleSave(); renderQueue();
  }
  function jumpHunk(id) {
    activeHunk = id;
    document.querySelectorAll('.hunk-row').forEach(row => row.classList.toggle('active',Number(row.dataset.hunk) === id));
    for (const name of ['base-text','local-text','remote-text']) {
      const pane = $(name);
      pane.querySelectorAll('[data-hunks]').forEach(row => row.classList.toggle('hunk-focus',row.dataset.hunks.split(' ').includes(String(id))));
      const target = pane.querySelector(`[data-hunks~="${id}"]`);
      if (target) pane.scrollTop = Math.max(0,target.offsetTop - pane.clientHeight / 3);
    }
  }
  function renderSources() {
    for (const [side,id] of [['Base','base-text'],['Local','local-text'],['Remote','remote-text']]) {
      const pane = $(id); pane.replaceChildren(); pane.dataset.side = side.toLowerCase();
      const rows = diff.makeLineRows(current[side],current.hunks,side);
      if (!rows.length) pane.append(make('div',current.text ? '（空文本）' : '（只读非文本文件）','source-empty'));
      for (const line of rows) {
        const row = make('div',undefined,line.type === 'anchor' ? 'diff-anchor' : 'source-line' + (line.changed ? ' changed change-' + line.kind : ''));
        if (line.hunks.length) {
          row.dataset.hunks = line.hunks.join(' ');
          row.addEventListener('click',() => jumpHunk(line.hunks[0]));
        }
        const number = make('span',line.type === 'line' ? String(line.number) : '·','source-line-number'); number.setAttribute('aria-hidden','true');
        const text = make('span',line.type === 'line' ? line.text : line.label,'source-line-text');
        row.append(number,text); pane.append(row);
      }
    }
  }
  function sourceRange(h,side) {
    const start = h[side + 'Start'],end = h[side + 'End'];
    return start === end ? `第 ${start + 1} 行前` : start + 1 === end ? `第 ${start + 1} 行` : `第 ${start + 1}–${end} 行`;
  }
  function renderHunks() {
    $('hunk-list').replaceChildren(); $('hunk-count').textContent = current.remote_deleted ? '云端删除需要单独决定；保留时使用本地版本或下方编辑结果。' : `${current.hunks.length} 处差异，点击可定位三份来源`;
    if (current.remote_deleted) return;
    for (const h of current.hunks) {
      const row = make('div',undefined,'hunk-row' + (h.id === activeHunk ? ' active' : '')); row.dataset.hunk = h.id;
      const heading = make('div',undefined,'hunk-row-heading');
      const jump = make('button',`差异 ${h.id + 1}，原版${sourceRange(h,'Base')}`,'hunk-jump');
      jump.title = '定位到三份来源的对应文本'; jump.addEventListener('click',() => jumpHunk(h.id));
      const group = make('div',undefined,'hunk-choices');
      for (const [key,label] of [['Local','采用本地'],['Remote','采用云端'],['Base','还原原版']]) {
        const button = make('button',label,'choice' + ((choices.get(h.id) || 'Local') === key ? ' active' : '')); button.disabled = manual || queue.running || !current.text || record()?.status === 'queued';
        button.addEventListener('click',() => { choices.set(h.id,key); rebuild(); renderHunks(); jumpHunk(h.id); }); group.append(button);
      }
      heading.append(jump,group); row.append(heading);
      const context = make('div',undefined,'hunk-context');
      for (const [side,label] of [['Base','共同原版'],['Local','本地'],['Remote','云端']]) {
        const cell = make('section',undefined,'hunk-context-cell context-' + side.toLowerCase());
        let delta = '';
        if (side !== 'Base') delta = h.WholeDocument ? '，整份比较' : `，+${h[side+'Added']} / −${h[side+'Removed']} 行`;
        cell.append(make('div',`${label} ${sourceRange(h,side)}${delta}`,'hunk-context-label'),make('pre',diff.snippet(h[side]),'hunk-snippet'));
        context.append(cell);
      }
      row.append(context); $('hunk-list').append(row);
    }
  }
  function showConflict(r) {
    current = r.input; current.hunks ||= []; choices = r.choices; manual = r.manual; activeHunk = null;
    $('merge').hidden = false; view(activeView === 'settings' ? 'conflicts' : activeView);
    $('merge-name').textContent = r.name; $('merge-version').textContent = current.remote_deleted ? '云端文件已不在原目录，本地修改已保留。' : `本地代次 ${current.generation}，云端修订 ${current.remote_revision || '下载观测'}`;
    $('merge-hint').hidden = Boolean(current.remote_deleted);
    renderSources(); $('merged-text').value = r.text; recoveryControls(); renderHunks();
    $('copy-paths').replaceChildren(); $('copy-paths').hidden = true;
    draftState(r.status === 'queued' ? (r.newerDraft ? '新编辑仍在页面中，请复制保存' : '已在本机接受，等待同步') : r.seq !== r.savedSeq ? '页面草稿已保留' : current.has_draft ? '已载入本机草稿' : '草稿未修改');
    renderConflicts();
  }
  async function openConflict(id, reload = false) {
    const requestSeq = ++openSeq;
    clearTimeout(draftTimer);
    if (record()?.seq !== record()?.savedSeq && !queue.running) saveDraft();
    const existing = queue.records.get(id);
    if (existing?.input && (!reload || existing.status === 'queued')) { showConflict(existing); return; }
    try {
      const data = await api(`/api/conflicts/${encodeURIComponent(id)}`);
      let cached; try { cached = JSON.parse(localStorage.getItem(`draft:${id}`)); } catch (_) {}
      const r = queue.load(data,cached); stash(r);
      if (requestSeq !== openSeq) return;
      showConflict(r);
      if (!data.text) message(data.remote_deleted ? '该文件不是可编辑文本。可直接保留本地版本并重新上传，或确认跟随云端删除。' : '该文件不是可编辑文本。原版和双方副本仍保留，请保存副本后处理。');
    } catch(e) { if (requestSeq === openSeq) message(e.message,true); }
  }
  $('refresh-conflict').addEventListener('click',() => { if (current && !queue.running) openConflict(current.id,true); });
  $('review-sources').addEventListener('click',() => { if (current && queue.review(current.id)) { stash(); recoveryControls(); renderConflicts(); draftState('保留草稿，已核对当前来源'); } });
  $('export-copies').addEventListener('click',async () => {
    if (!current || queue.running) return;
    const id = current.id; $('export-copies').disabled = true;
    try {
      if (!(await saveDraft())) return;
      const copies = await api(`/api/conflicts/${encodeURIComponent(id)}/copies`,'POST',{});
      if (current?.id !== id) return;
      const box = $('copy-paths'); box.replaceChildren(make('strong','本地副本已保存'),make('p','可复制以下目录到文件管理器查看。编辑这些副本不会改写受保护的同步数据。'));
      const paths = make('dl');
      for (const [key,label] of [['directory','副本目录'],['base','共同原版'],['local','本地内容'],['remote','云端内容'],['draft','合并草稿'],['manifest','校验清单']]) { if (copies[key]) paths.append(make('dt',label),make('dd',copies[key])); }
      box.append(paths); box.hidden = false;
    } catch(e) { message(e.message,true); } finally { recoveryControls(); }
  });
  $('reset-choices').addEventListener('click',async () => { const id = current?.id; if (!id || queue.running || !(await saveDraft()) || current?.id !== id || queue.running) return; record().manual = manual = false; record().choices = choices = new Map(); rebuild(); renderHunks(); });
  function batchChanged(r) {
    if (r) {
      if (r.status === 'queued' && !r.newerDraft) { try { localStorage.removeItem(`draft:${r.id}`); } catch (_) {} }
      else stash(r);
    }
    renderConflicts(); recoveryControls();
    if (r && r.id === current?.id) {
      draftState(r.status === 'queued' ? r.decision === 'accept_delete' ? '已跟随云端删除' : r.newerDraft ? '新编辑仍在页面中，请复制保存' : '已在本机接受，等待同步' : r.status === 'submitting' ? '正在保存并提交本次草稿…' : '草稿已保留');
      // Batch replies only change controls and status; they never replace editor text.
    }
    if (current) renderHunks();
  }
  async function submitFiles(ids) {
    if (queue.running) return;
    clearTimeout(draftTimer); stash();
    await queue.submit(draftIO,batchChanged,ids);
    message('本次提交已处理，请查看每份文件的结果。本机接受的结果仍需等待云端确认。');
    await refresh();
    const r = record(); if (r && r.seq !== r.savedSeq && r.status !== 'queued') scheduleSave();
  }
  $('resolve').addEventListener('click',() => { if (current && queue.canSubmit(record())) submitFiles([current.id]); });
  async function decideDeletion(action) {
    const r = record(); if (queue.running || !queue.canDecide(r)) return;
    if (action === 'accept_delete' && !window.confirm('确定跟随云端删除此文件？本地文件和待同步的本地修改将被删除。可先保存副本。')) return;
    clearTimeout(draftTimer); stash(r);
    await queue.decide(r.id,action,draftIO,batchChanged);
    message(r.status === 'queued' ? action === 'accept_delete' ? '已接受云端删除。' : '已保留本地版本，等待重新上传并确认云端。' : '本次决定尚未接受，草稿已保留；请查看文件状态。',r.status !== 'queued');
    await refresh();
  }
  $('restore-local').addEventListener('click',() => decideDeletion('restore_local'));
  $('accept-delete').addEventListener('click',() => decideDeletion('accept_delete'));
  $('submit-selected').addEventListener('click',() => submitFiles());
  $('select-reviewed').addEventListener('click',() => { for (const r of queue.records.values()) queue.select(r.id,true); renderQueue(); });
  $('clear-selection').addEventListener('click',() => { for (const r of queue.records.values()) queue.select(r.id,false); renderQueue(); });
  $('later').addEventListener('click',async () => { if (await saveDraft()) { $('merge').hidden = true; message('草稿已保留，可随时从列表继续处理。'); } });
  window.addEventListener('beforeunload',event => { const unsaved = [...queue.records.values()].filter(r => r.input?.text && r.seq !== r.savedSeq); for (const r of unsaved) stash(r); if (unsaved.length) { event.preventDefault(); event.returnValue = ''; } });
  async function start() {
    const fragment = new URLSearchParams(location.hash.slice(1)); const bootstrap = fragment.get('bootstrap'); history.replaceState(null,'',location.pathname + location.search);
    try { const session = bootstrap ? await api('/api/session','POST',{bootstrap}) : await api('/api/session'); csrf = session.csrf; const config = await loadConfig(); await refresh(); authMonitor.restore(); const params = new URLSearchParams(location.search), id = params.get('conflict'); if (params.get('view') === 'conflicts' || id) view('conflicts'); if (id) await openConflict(id); else if (params.get('view') !== 'conflicts' && !config.mounts.length) view('settings'); scheduleStatusPoll(); } catch (_) { showSessionError(); }
  }
  start();
})();
