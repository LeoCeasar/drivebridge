'use strict';
(() => {
  const $ = id => document.getElementById(id);
  let csrf = '', current = null, choices = new Map(), manual = false, editSeq = 0, savedSeq = 0;
  let draftTimer = null, draftChain = Promise.resolve(), authFlow = '', busy = false;
  const make = (tag, text, className) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (className) e.className = className; return e; };
  function message(text, error = false) { $('message').textContent = text; $('message').classList.toggle('error', error); $('message').hidden = !text; }
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
  function bytes(text) { const data = new TextEncoder().encode(text); let result = ''; for (let i = 0; i < data.length; i += 8192) result += String.fromCharCode(...data.subarray(i, i + 8192)); return btoa(result); }
  function view(name) { for (const key of ['workspace', 'settings']) $(key).hidden = key !== name; document.querySelectorAll('[data-view]').forEach(e => e.classList.toggle('active', e.dataset.view === name)); $('page-label').textContent = name === 'settings' ? '账号与目录' : '文件与同步'; }
  document.querySelectorAll('[data-view]').forEach(e => e.addEventListener('click', () => view(e.dataset.view)));
  for (const id of ['setup-shortcut', 'start-setup']) $(id).addEventListener('click', () => view('settings'));
  function cloudLabel(state) { const value = state.cloud_state; const labels = {synced:'已确认',queued:'等待同步',uploading:'正在上传',merging:'正在合并',awaiting_verification:'等待回读核验',conflict:'需要合并',blocked:'已阻塞',paused:'已暂停',offline:'离线'}; return labels[value] || (state.running ? '等待确认' : '挂载未启动'); }
  function renderMounts(mounts = []) {
    $('mount-rows').replaceChildren(); $('mount-empty').hidden = mounts.length > 0;
    for (const mount of mounts) {
      const state = mount.state || {}, row = make('tr'); const name = make('td'); name.append(make('span', mount.name, 'mount-name'), make('span', mount.mountpoint, 'mount-path'));
      const local = make('td'); local.append(make('span', state.local_saved ? '已持久保存' : (state.running ? '检查中或仍有本机写入' : '挂载未启动'), 'state-tag' + (state.local_saved ? '' : ' pending')));
      const cloud = make('td', cloudLabel(state)); const queue = make('td', `队列 ${state.queued || 0}，在途 ${state.inflight || 0}，冲突 ${state.conflicts || 0}`); const action = make('td'); const button = make('button', state.running ? '安全停止' : '启动挂载', 'button small');
      button.addEventListener('click', async () => { button.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/${state.running ? 'stop' : 'start'}`, 'POST', {}); await refresh(); } catch (e) { message(e.message, true); } finally { button.disabled = false; } }); action.append(button); row.append(name, local, cloud, queue, action); $('mount-rows').append(row);
    }
  }
  function renderConflicts(conflicts = []) {
    $('conflict-count').textContent = conflicts.length; $('conflict-list').replaceChildren(); $('conflict-empty').hidden = conflicts.length > 0;
    for (const c of conflicts) { const button = make('button', c.name || '待合并文件', 'conflict-item' + (current?.id === c.id ? ' selected' : '')); button.title = c.name || ''; button.addEventListener('click', () => openConflict(c.id)); $('conflict-list').append(button); }
  }
  async function refresh() { try { const status = await api('/api/status'); renderMounts(status.mounts); renderConflicts(status.conflicts); $('connection').textContent = '已连接本机服务'; } catch (e) { $('connection').textContent = '本机连接暂不可用'; message(e.message, true); } }
  $('refresh').addEventListener('click', refresh);
  function options(select, items, label) { select.replaceChildren(); if (!items.length) { const empty = make('option', label); empty.value = ''; select.append(empty); } for (const item of items) { const option = make('option', item.name); option.value = item.id; select.append(option); } }
  async function loadConfig() {
    const config = await api('/api/config'); options($('profile-select'), config.profiles.filter(p => p.authorized), '先完成账号授权'); options($('remote-select'), config.remotes, '先添加共享目录'); $('profiles').replaceChildren();
    for (const p of config.profiles) { const row = make('div', undefined, 'profile-row'); const title = make('div', p.name); title.append(make('small', `${p.brand === 'lark' ? 'Lark' : '飞书'}，${p.authorized ? '已授权' : '待授权'}`)); const button = make('button', p.authorized ? '重新授权' : '授权', 'button small'); button.addEventListener('click', async () => { button.disabled = true; try { const flow = await api('/api/auth/start', 'POST', {profile_id:p.id}); authFlow = flow.id; $('auth-flow').hidden = false; $('auth-link').href = flow.url; $('auth-state').textContent = flow.expires_at ? `请在 ${new Date(flow.expires_at).toLocaleTimeString()} 前完成。` : '等待授权完成；关闭页面不会暴露凭据。'; } catch (e) { message(e.message, true); } finally { button.disabled = false; } }); row.append(title, button); $('profiles').append(row); if (p.authorized && config.can_recover) { const recovery = make('button', '停止相关挂载并重新授权，保留待上传数据', 'text-button recovery-auth'); recovery.addEventListener('click', async () => { recovery.disabled = true; try { const flow = await api('/api/auth/start','POST',{profile_id:p.id,recovery:true}); authFlow = flow.id; $('auth-flow').hidden = false; $('auth-link').href = flow.url; $('auth-state').textContent = '相关挂载已进入保留待上传数据的恢复流程；请完成原身份授权。'; } catch (e) { message(e.message,true); } finally { recovery.disabled = false; } }); $('profiles').append(recovery); } }
    return config;
  }
  for (const [id, action] of [['profile-form','profile'],['remote-form','remote'],['mount-form','mount']]) $(id).addEventListener('submit', async event => { event.preventDefault(); const form = event.currentTarget; const button = form.querySelector('button'); button.disabled = true; const data = Object.fromEntries(new FormData(form)); data.action = action; try { await api('/api/config', 'POST', data); message(action === 'mount' ? '挂载配置已保存，可在“文件与同步”中启动。' : '配置已保存。'); form.reset(); await loadConfig(); await refresh(); } catch (e) { message(e.message, true); } finally { button.disabled = false; } });
  $('auth-complete').addEventListener('click', async () => { if (!authFlow) return; $('auth-complete').disabled = true; $('auth-state').textContent = '正在等待授权结果并核验身份…'; try { await api('/api/auth/complete','POST',{flow_id:authFlow}); $('auth-flow').hidden = true; authFlow = ''; message('授权成功，可以选择共享目录。'); await loadConfig(); } catch (e) { $('auth-state').textContent = e.message; message(e.message, true); } finally { $('auth-complete').disabled = false; } });
  function stash() { if (!current) return; try { localStorage.setItem(`draft:${current.id}`, JSON.stringify({text:$('merged-text').value, generation:current.generation, revision:current.remote_revision})); } catch (_) { /* The editor and durable backend draft remain the primary copies. */ } }
  function draftState(text) { $('draft-state').textContent = text; }
  async function saveDraft() {
    if (!current) return true; clearTimeout(draftTimer); const id = current.id, seq = editSeq, content = $('merged-text').value; stash();
    let success = false; draftChain = draftChain.catch(() => {}).then(async () => { try { await api(`/api/conflicts/${encodeURIComponent(id)}/draft`,'PUT',{content:bytes(content)}); if (current?.id === id && seq === editSeq) { savedSeq = seq; draftState('草稿已保存到本机'); try { localStorage.removeItem(`draft:${id}`); } catch (_) {} } success = true; } catch (e) { if (current?.id === id) { draftState('尚未保存到本机，请保持页面打开'); message(e.message + ' 当前编辑仍在页面中，请恢复连接后保存。', true); } } }); await draftChain; return success;
  }
  function edited() { editSeq++; manual = true; stash(); draftState('正在编辑，稍后自动保存'); document.querySelectorAll('.choice').forEach(e => e.disabled = true); clearTimeout(draftTimer); draftTimer = setTimeout(saveDraft, 650); }
  $('merged-text').addEventListener('input', edited); $('save-draft').addEventListener('click', saveDraft);
  function baseLines(text) { const parts = text.match(/[^\n]*\n|[^\n]+$/g); return parts || []; }
  function rebuild() { if (!current) return; const source = baseLines(current.Base); let cursor = 0, text = ''; for (const h of current.hunks) { text += source.slice(cursor,h.BaseStart).join(''); text += h[choices.get(h.id) || 'Local']; cursor = h.BaseEnd; } text += source.slice(cursor).join(''); $('merged-text').value = text; editSeq++; stash(); draftState('差异选择已更新，稍后自动保存'); clearTimeout(draftTimer); draftTimer = setTimeout(saveDraft,650); }
  function renderHunks() { $('hunk-list').replaceChildren(); $('hunk-count').textContent = `${current.hunks.length} 处差异`; for (const h of current.hunks) { const row = make('div', undefined, 'hunk-row'); row.append(make('span', `差异 ${h.id + 1}，原版第 ${h.BaseStart + 1} 行`)); const group = make('div',undefined,'hunk-choices'); for (const [key,label] of [['Local','采用本地'],['Remote','采用云端'],['Base','还原原版']]) { const b = make('button',label,'choice' + ((choices.get(h.id) || 'Local') === key ? ' active' : '')); b.disabled = manual; b.addEventListener('click', () => { choices.set(h.id,key); rebuild(); renderHunks(); for (const id of ['base-text','local-text','remote-text']) $(id).scrollTop = h.BaseStart * 22; }); group.append(b); } row.append(group); $('hunk-list').append(row); } }
  async function openConflict(id) {
    if (current && editSeq !== savedSeq && !(await saveDraft())) return;
    try { const data = await api(`/api/conflicts/${encodeURIComponent(id)}`); current = data; choices = new Map(); editSeq = 0; savedSeq = 0; manual = data.has_draft; $('merge').hidden = false; view('workspace'); $('merge-name').textContent = data.name; $('merge-version').textContent = `本地代次 ${data.generation}，云端修订 ${data.remote_revision || '下载观测'}`; $('base-text').textContent = data.Base; $('local-text').textContent = data.Local; $('remote-text').textContent = data.Remote; $('merged-text').value = data.has_draft ? data.Draft : data.Local; $('merged-text').disabled = !data.text; $('resolve').disabled = !data.text; $('save-draft').disabled = !data.text;
      try { const cached = JSON.parse(localStorage.getItem(`draft:${id}`)); if (cached && cached.text !== $('merged-text').value) { $('merged-text').value = cached.text; manual = true; editSeq++; draftState('恢复了此浏览器暂存内容，请保存草稿'); } else draftState(data.has_draft ? '已载入本机草稿' : '草稿未修改'); } catch (_) { draftState(data.has_draft ? '已载入本机草稿' : '草稿未修改'); }
      if (!data.text) message('该文件不是可编辑文本。原版和双方副本仍保留，请通过恢复入口另存后处理。'); renderHunks(); await refresh();
    } catch (e) { message(e.message,true); }
  }
  $('reset-choices').addEventListener('click', async () => { if (!current || !(await saveDraft())) return; manual = false; choices = new Map(); rebuild(); renderHunks(); });
  $('resolve').addEventListener('click', async () => { if (!current || busy) return; busy = true; $('resolve').disabled = true; try { if (!(await saveDraft())) return; await api(`/api/conflicts/${encodeURIComponent(current.id)}/resolve`,'POST',{conflict_id:current.id,expected_generation:current.generation,expected_remote_revision:current.remote_revision,content:bytes($('merged-text').value)}); message('合并结果已在本机接受，云端确认请查看同步状态。'); $('merge').hidden = true; current = null; await refresh(); } catch (e) { draftState('草稿已保留'); message(e.status === 409 ? '输入版本已变化，草稿已保留。请刷新差异并重新核对后提交。' : e.message,true); } finally { busy = false; $('resolve').disabled = !current?.text; } });
  $('later').addEventListener('click', async () => { if (await saveDraft()) { $('merge').hidden = true; message('草稿已保留，可随时从左侧继续处理。'); } });
  window.addEventListener('beforeunload', event => { if (current && editSeq !== savedSeq) { stash(); event.preventDefault(); event.returnValue = ''; } });
  async function start() {
    const fragment = new URLSearchParams(location.hash.slice(1)); const bootstrap = fragment.get('bootstrap'); history.replaceState(null,'',location.pathname + location.search);
    try { const session = bootstrap ? await api('/api/session','POST',{bootstrap}) : await api('/api/session'); csrf = session.csrf; const config = await loadConfig(); await refresh(); const id = new URLSearchParams(location.search).get('conflict'); if (id) await openConflict(id); else if (!config.mounts.length) view('settings'); setInterval(refresh,5000); } catch (_) { showSessionError(); }
  }
  start();
})();
