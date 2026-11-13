'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const diff = window.DriveBridgeDiff;
  const recovery = window.DriveBridgeRecovery;
  const queue = window.DriveBridgeBatch.createQueue();
  let activeHunk = null, openSeq = 0, activeView = 'workspace';
  let csrf = '', current = null, choices = new Map(), manual = false, mergeOpen = false;
  let draftTimer = null, authFlow = '', authProfile = '', authAutomatic = false, authConfigFlow = '', authRetry = null;
  let messageOwner = '', statusFlight = null, statusDelay = 5000, statusPartial = false, sessionLost = false, networkFailures = 0;
  // The page never guesses the platform: the backend names its own file manager.
  let openLabel = '在文件管理器中打开', canOpen = false;
  const fileManagerLabels = {windows:'在资源管理器中打开',darwin:'在 Finder 中打开'};
  const titles = {workspace:'文件与同步',conflicts:'冲突与合并',settings:'账号与目录',history:'修改历史'};
  let currentPlatform = '';
  const record = () => current && queue.records.get(current.id);
  const deletionDecision = input => Boolean(input?.remote_deleted || input?.local_delete_pending);
  const deletedLabel = r => r?.input?.local_delete_pending ? '已确认本地删除，上传已取消' : '已跟随云端删除';
  const make = (tag, text, className) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (className) e.className = className; return e; };
  function message(text, error = false, owner = '') { messageOwner = owner; $('message').textContent = text; $('message').classList.toggle('error', error); $('message').hidden = !text; }
  function removalHelp(error,mount) {
    message(error.message,true);
    if (!['mount_unsynced','history_pending','mount_in_use'].includes(error.code)) return;
    const actions=make('div',undefined,'merge-actions');
    const inspect=make('button',error.code==='history_pending'?'查看历史归档':mount.state?.conflicts?'处理冲突':'查看同步任务','button small');
    inspect.addEventListener('click',()=>{view(error.code==='history_pending'?'history':mount.state?.conflicts?'conflicts':'workspace');if(error.code!=='history_pending'&&!mount.state?.conflicts)$('sync-issues').scrollIntoView({block:'start'});});actions.append(inspect);
    if(!mount.status_warning&&(mount.archived||!mount.state?.running)){
      const resume=make('button',mount.archived?'恢复并启动同步':'启动同步','button small');
      resume.addEventListener('click',async()=>{resume.disabled=true;try{await api(`/api/mounts/${encodeURIComponent(mount.id)}/${mount.archived?'restore_start':'start'}`,'POST',{});message('同步已启动，待上传与冲突处理完成后可重新删除。');await loadConfig();await refresh();}catch(e){message(e.message,true);}finally{resume.disabled=false;}});actions.append(resume);
    }
    $('message').append(actions);
  }
  function showSessionError(text) {
    sessionLost = true; $('app-shell').hidden = true; $('session-error').hidden = false;
    if (text) $('session-error-text').textContent = text;
    $('connection').textContent = '本机会话已失效';
  }
  async function api(route, method = 'GET', body) {
    const headers = {};
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
    let response;
    try { response = await fetch(route, {method, headers, credentials: 'same-origin', cache: 'no-store', body: body === undefined ? undefined : JSON.stringify(body)}); }
    catch (_) { const error = new Error('无法连接本机 DriveBridge，请确认它仍在运行。'); error.code = 'network'; throw error; }
    let data = {}; try { data = await response.json(); } catch (_) { /* Stable fallback below. */ }
    if (!response.ok) { if (response.status === 403 && data.code !== 'history_identity_changed') showSessionError(); const error = new Error(data.message || '本机服务暂时不可用，请稍后重试。'); error.status = response.status; error.code = data.code; error.retryable = data.retryable === true; throw error; }
    return data;
  }
  // Copying is a convenience; when the browser refuses, the path stays selectable on screen.
  async function copyText(text, button) {
    let ok = false;
    try { await navigator.clipboard.writeText(text); ok = true; }
    catch (_) { try { const area = make('textarea'); area.value = text; area.className = 'visually-hidden'; document.body.append(area); area.select(); ok = document.execCommand('copy'); area.remove(); } catch (__) { /* Reported below. */ } }
    if (!ok) { message('浏览器未允许复制，请手动选择路径后复制。', true, 'copy'); return; }
    if (button) { button.classList.toggle('done', true); button.title = '已复制'; setTimeout(() => { button.classList.toggle('done', false); button.title = '复制路径'; }, 1600); }
  }
  const conversionRules = window.DriveBridgeRules?.create({document,api,confirm:text=>window.confirm(text)});
  const historyView = window.DriveBridgeHistory?.create({document,api,onMessage:message,confirm:text=>window.confirm(text)});
  const discovery = window.DriveBridgeDiscovery.create({document,api,onChanged:() => refresh(),copy:copyText,openLabel:() => openLabel,canOpen:() => canOpen});
  const authMonitor = window.DriveBridgeAuth.create({api,storage:window.localStorage,onProgress:renderAuthProgress,onError:(error,terminal) => {
    $('auth-flow').hidden = false;
    $('auth-state').textContent = terminal ? '上次授权进度会话已结束；已保存账号和挂载状态以当前页面为准。' : '进度连接暂未更新；后台会继续等待授权，稍后自动重试。';
    $('auth-complete').hidden = terminal;
  }});
  const authErrors = {
    auth_identity_changed:'这次登录使用的飞书账号与已保存的账号不是同一个，未做任何更改。请用原账号重新授权，或把新账号添加为另一个账号。',
    auth_reconcile_failed:'核验已保存的授权失败，应用配置与待上传数据已保留，请重试或重启 DriveBridge。',
    local_busy:'本机正在处理其他操作，请稍后重试。'
  };
  function showAuthFlow(flow,profileID) {
    authFlow = flow.id; authProfile = profileID; authAutomatic = flow.auto_complete === true; authConfigFlow = ''; authRetry = null;
    // A reconciled flow means the earlier authorization had already finished: there is no page to open.
    const linkless = flow.kind === 'reconciled' || !flow.url;
    $('auth-flow').hidden = false; $('auth-link').hidden = linkless; if (!linkless) $('auth-link').href = flow.url;
    $('auth-retry').hidden = true; $('auth-state').classList.toggle('error',false);
    $('auth-complete').hidden = authAutomatic;
    $('auth-state').textContent = linkless ? '已确认之前的授权，正在继续登录…' : authAutomatic ? '请在飞书页面完成授权，后台将自动继续，无需返回确认。' : '请在飞书页面授权后检查结果。';
    if (authAutomatic) authMonitor.track(flow,profileID);
  }
  function authFailed(text,retry) {
    $('auth-flow').hidden = false; $('auth-link').hidden = true; $('auth-complete').hidden = true;
    $('auth-state').textContent = text; $('auth-state').classList.toggle('error',true);
    authRetry = retry; $('auth-retry').hidden = !retry;
    message(text,true);
  }
  async function startAuth(profileID, recoveryStart) {
    try { showAuthFlow(await api('/api/auth/start','POST',recoveryStart ? {profile_id:profileID,recovery:true} : {profile_id:profileID}),profileID); }
    catch (e) { authFailed(authErrors[e.code] || e.message,e.retryable || e.code === 'auth_reconcile_failed' ? () => startAuth(profileID,recoveryStart) : null); }
  }
  $('auth-retry').addEventListener('click',async () => { const retry = authRetry; if (retry) { $('auth-retry').hidden = true; await retry(); } });
  async function renderAuthProgress(progress) {
    authFlow = progress.flow_id; authProfile = progress.profile_id; authAutomatic = true;
    $('auth-flow').hidden = progress.state === 'ready'; $('auth-complete').hidden = true; $('auth-retry').hidden = true; $('auth-state').classList.toggle('error',false);
    $('auth-link').hidden = progress.state !== 'waiting_authorization' || (!progress.flow?.url && !$('auth-link').href);
    if (progress.flow?.url) $('auth-link').href = progress.flow.url;
    const labels = {waiting_authorization:'请在飞书页面完成授权，后台会自动继续。',opening_login:'授权已验证，正在自动打开应用登录窗口…',waiting_login:'请在应用窗口登录飞书；登录后自动挂载，无需选择目录。',ready:'账号盘已自动挂载。'};
    $('auth-state').textContent = labels[progress.state] || '正在接续登录…';
    if (progress.state === 'failed') {
      const errors = {mount_archived:'账号盘已移除，请先在“已移除挂载”中恢复配置。',expired:'授权或登录等待已超时，可以重新发起登录。',canceled:'本次登录已取消。',partial:'目录入口尚未完整读取，可重新扫描后检查挂载。',needs_runtime:'请安装 Microsoft Edge WebView2 Runtime，再从账号盘区域重新打开登录窗口。',unsupported:'应用内登录组件不可用，请检查系统版本和完整安装包。',needs_browser:'应用登录组件不可用，请检查是否安装完整应用。',needs_login:'网页登录尚未完成，可从账号盘区域重新打开登录窗口。',unauthorized:'用户授权尚未完成，请重新发起授权。',identity_changed:authErrors.auth_identity_changed,auth_identity_changed:authErrors.auth_identity_changed,auth_reconcile_failed:authErrors.auth_reconcile_failed,driver_unavailable:'挂载驱动不可用，请检查系统驱动。'};
      const reason = errors[progress.error] || '自动登录或挂载尚未完成，请查看账号盘状态后重试。';
      $('auth-state').textContent = reason + ' 已保存的授权和同步数据会保留。';
      $('auth-state').classList.toggle('error',true);
      if (progress.error === 'auth_reconcile_failed' && progress.profile_id) { const id = progress.profile_id; authRetry = () => startAuth(id,false); $('auth-retry').hidden = false; }
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
    const conflicts = name === 'conflicts';
    for (const key of ['workspace','settings','history']) $(key).hidden = key !== (conflicts ? 'workspace' : name);
    if (name === 'history') historyView?.load();
    if (name === 'settings') conversionRules?.load();
    document.querySelectorAll('[data-view]').forEach(e => e.classList.toggle('active', e.dataset.view === name));
    $('page-label').textContent = titles[name]; document.title = `${titles[name]} · DriveBridge`;
    $('workspace-description').textContent = conflicts ? '核对每份文件的差异，保留草稿并提交已选结果。' : '本机保存立即生效；后台每5分钟同步各文件的最新保存版本，上传期间的新修改留待下一轮。';
    $('sync-issues').hidden = conflicts || !$('sync-issue-list').children.length;
    $('mount-panel').hidden = conflicts; $('conflict-summary').hidden = conflicts; $('setup-shortcut').hidden = conflicts; $('conflict-area').hidden = !conflicts;
  }
  document.querySelectorAll('[data-view]').forEach(e => e.addEventListener('click', () => view(e.dataset.view)));
  for (const id of ['setup-shortcut', 'start-setup']) $(id).addEventListener('click', () => view('settings'));
  $('sync-attention').addEventListener('click',()=>{view('workspace');$('sync-issues').scrollIntoView?.({block:'start',behavior:'smooth'});});
  $('conflict-summary-go').addEventListener('click', () => view('conflicts'));
  $('session-reload').addEventListener('click', () => location.reload());
  // Menus close on outside click and Escape without a framework.
  function closeMenus(except) { document.querySelectorAll('.more-menu[open]').forEach(menu => { if (menu !== except) menu.open = false; }); }
  document.addEventListener?.('click', event => closeMenus(event.target.closest?.('.more-menu')));
  document.addEventListener?.('keydown', event => { if (event.key === 'Escape') closeMenus(); });
  function cloudLabel(state) { return recovery.cloudChip(state,state.running)[0]; }
  function chip(text, tone) { return make('span',text,`chip ${tone}`); }
  function meter(label, value, max, hint) {
    const box = make('div',undefined,'meter'), bar = make('progress');
    bar.max = Math.max(max, 1); bar.value = Math.min(Math.max(value, 0), bar.max); bar.className = value > max ? 'over' : ''; bar.setAttribute('aria-label',label);
    box.append(make('span',label),bar); if (hint) box.append(make('span',hint));
    return box;
  }
  function mountMeta(mount, state) {
    const meta = make('div',undefined,'mount-meta'), budget = mount.cache_max_bytes, retention = recovery.retentionLabel(mount.cache_max_age);
    if (budget > 0 && typeof state.cache_bytes === 'number') meta.append(meter(`正文缓存 ${recovery.formatBytes(state.cache_bytes)} / ${recovery.formatBytes(budget)}（软上限）`,state.cache_bytes,budget,retention));
    else if (budget > 0) meta.append(make('span',`正文缓存上限 ${recovery.formatBytes(budget)}（软上限）${retention ? '，' + retention : ''}`,'mount-path'));
    if (state.cloud_sync_interval) meta.append(make('span','云端每5分钟同步最新保存版本；所有保存仍保留修改历史。','mount-path'));
    const index = state.directory_index;
    if (index && index.known > 0) meta.append(meter(`目录清单 ${index.cached || 0} / ${index.known}`,index.cached || 0,index.known,index.failed ? '部分更新失败，保留旧清单' : index.refreshing ? '后台更新中' : ''));
    return meta;
  }
  function mountActions(mount, state, stale, canRetry) {
    const action = make('div',undefined,'actions');
    const toggle = make('button', mount.archived ? '恢复并启动同步' : state.running ? '暂停同步' : '启动挂载', 'button small' + (!mount.archived && !state.running ? ' primary' : '')); toggle.disabled = stale;
    toggle.addEventListener('click', async () => { toggle.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/${mount.archived ? 'restore_start' : state.running ? 'stop' : 'start'}`, 'POST', {}); if (mount.archived) message('挂载已恢复并启动，后台将继续处理同步任务。'); await loadConfig(); await refresh(); } catch (e) { message(e.message, true); } finally { toggle.disabled = false; } });
    action.append(toggle);
    if (mount.archived) { action.append(moreMenu(mount,stale)); return action; }
    if (canOpen && state.running) {
      const open = make('button', openLabel, 'button small'); open.disabled = stale;
      open.addEventListener('click', async () => { open.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/open`, 'POST', {}); } catch (e) { message(e.message, true); } finally { open.disabled = stale; } });
      action.append(open);
    }
    if (canRetry && state.running && recovery.hasRetryWork(state)) {
      const retry = make('button','重试同步','button small'); retry.disabled = stale || !recovery.canRetry(mount,canRetry); retry.title = '重新核验并重试待同步项目';
      retry.addEventListener('click',async () => { retry.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/retry`,'POST',{}); message('已请求重新核验并重试同步，请查看云端状态。'); await refresh(); } catch(e) { message(e.message,true); } finally { retry.disabled = stale || !recovery.canRetry(mount,canRetry); } });
      action.append(retry);
    }
    action.append(moreMenu(mount, stale));
    return action;
  }
  // Rare or destructive actions live here, and removal needs a second, explained click.
  function moreMenu(mount, stale) {
    const menu = make('details',undefined,'more-menu'), panel = make('div',undefined,'more-panel');
    const copyPath = make('button','复制挂载位置','menu-item'); copyPath.addEventListener('click',() => copyText(mount.mountpoint,null));
    const copyID = make('button','复制挂载标识（内部 ID）','menu-item'); copyID.title = mount.id; copyID.addEventListener('click',() => copyText(mount.id,null));
    const remove = make('button','删除挂载及缓存…','menu-item danger'); remove.disabled = stale; remove.title = '删除本机挂载配置和缓存；保留飞书文件及账号 Git 历史。有未同步内容时阻止删除。';
    const confirm = make('div',undefined,'confirm-box'); confirm.hidden = true;
    const row = make('div',undefined,'row'), yes = make('button','确认删除','button small danger solid'), no = make('button','取消','button small');
    row.append(yes,no); confirm.append(make('p','删除将安全停止此挂载并清理本机配置和缓存。飞书上的文件不会被删除，账号 Git 历史保留。有未上传修改、冲突或待归档历史时将阻止删除，并提供处理入口。'),row);
    remove.addEventListener('click',() => { confirm.hidden = false; remove.hidden = true; });
    no.addEventListener('click',() => { confirm.hidden = true; remove.hidden = false; });
    yes.addEventListener('click',async () => { yes.disabled = true; try { await api(`/api/mounts/${encodeURIComponent(mount.id)}/delete`,'POST',{}); message('挂载配置与本地缓存已删除；飞书文件和 Git 历史仍保留。'); await loadConfig(); await refresh(); } catch(error) { removalHelp(error,mount); confirm.hidden = true; remove.hidden = false; if(error.code==='cleanup_pending'){await loadConfig();await refresh();} } finally { yes.disabled = false; } });
    panel.append(copyPath,copyID,remove,confirm); menu.append(make('summary','更多'),panel);
    return menu;
  }
  const rowCaches = new Map();
  // Keep existing positions; sort only new arrivals. Unchanged rows remain the
  // same DOM objects, including their menus, focus, selections and scroll state.
  function reconcileRows(id,items,build,signature = item => JSON.stringify(item)) {
    let cache = rowCaches.get(id); if (!cache) { cache = new Map(); rowCaches.set(id,cache); }
    const incoming = new Map(items.map(item => [item.id,item]));
    for (const key of cache.keys()) if (!incoming.has(key)) cache.delete(key);
    for (const item of [...items].sort((a,b) => (a.name || a.id).localeCompare(b.name || b.id) || a.id.localeCompare(b.id))) if (!cache.has(item.id)) cache.set(item.id,{});
    const rows = [];
    for (const [key,entry] of cache) {
      const item = incoming.get(key), sig = signature(item);
      const occupied = entry.row?.contains?.(document.activeElement) || entry.row?.querySelector?.('details[open]')?.open === true;
      if (!entry.row || (entry.sig !== sig && !occupied)) { entry.row = build(item); entry.sig = sig; }
      rows.push(entry.row);
    }
    const parent = $(id), currentRows = [...parent.children];
    if (rows.length === currentRows.length && rows.every((row,index) => row === currentRows[index])) return;
    if (!parent.insertBefore) { parent.replaceChildren(...rows); return; }
    // Remove obsolete rows first so replacing an earlier row never detaches a
    // later unchanged row (which may own the keyboard focus).
    const keep = new Set(rows);
    for (const row of currentRows) if (!keep.has(row)) row.remove();
    rows.forEach((row,index) => { if (parent.children[index] !== row) parent.insertBefore(row,parent.children[index] || null); });
  }
  function renderMounts(mounts = [], canRetry = false) {
    $('mount-empty').hidden = mounts.some(m => !m.archived); $('mount-panel').classList.toggle('is-empty',!$('mount-empty').hidden);
    $('archived-mount-panel').hidden = !mounts.some(m => m.archived);
    const renderMount = mount => {
      const stale = Boolean(mount.status_warning), known = !stale || Boolean(mount.status_updated_at);
      const state = mount.state || {}, row = make('tr'), prefix = stale ? '上次：' : '';
      const name = make('td'), head = make('div',undefined,'mount-head'), icon = make('span',undefined,'mount-icon'), id = make('div',undefined,'mount-id');
      icon.setAttribute('aria-hidden','true'); icon.append(make('span',undefined,'ico i-' + (mount.account_root ? 'drive' : 'folder')));
      const title = make('span',mount.display_name || mount.name,'mount-name'); if (mount.display_name && mount.display_name !== mount.name) title.title = `内部名称：${mount.name}`;
      id.append(title); if (mount.account_root) id.append(make('span','账号盘','mount-tag'));
      head.append(icon,id); name.append(head);
      const place = make('div',undefined,'path-line'), copy = make('button',undefined,'icon-button'); copy.title = '复制路径'; copy.setAttribute('aria-label','复制挂载位置'); copy.append(make('span',undefined,'ico i-copy'));
      copy.addEventListener('click',() => copyText(mount.mountpoint,copy)); place.append(make('span',mount.mountpoint,'mount-path'),copy); name.append(place);
      if (known && !mount.archived) name.append(mountMeta(mount,state));
      if (stale) {
        const reasons = {pending:'本机正在处理其他状态查询',auth_required:'账号需要重新授权',identity_changed:'账号身份需要重新核对',worker_unavailable:'挂载服务暂不可用'};
        const reason = reasons[mount.status_warning] || '本机状态更新暂未完成';
        name.append(make('small',known ? `${reason}；保留上次结果（${new Date(mount.status_updated_at).toLocaleTimeString()}）` : `${reason}；尚未取得完整状态`,'mount-path'));
      }
      const local = make('td'), saved = state.local_saved;
      const saveFailed = state.last_error === 'local_save_failed';
      local.append(!known ? chip('等待核对','info') : chip(prefix + (saveFailed ? '本机保存尚未确认' : saved ? '已持久保存' : (state.running ? '检查中或仍有本机写入' : '挂载未启动')), saveFailed ? 'error' : saved ? 'ok' : state.running ? 'pending' : 'stopped'));
      const cloud = make('td');
      if (!known) cloud.append(chip('等待核对','info'));
      else {
        const [label,tone] = recovery.cloudChip(state,state.running); cloud.append(chip(prefix + label,tone));
        if (state.last_error) cloud.append(make('small',recovery.errorLabels[state.last_error] || '上次操作未完成','mount-path'));
      }
      const counters = make('td',!known ? '暂不可读' : `${prefix}队列 ${state.queued || 0}，在途 ${state.inflight || 0}，冲突 ${state.conflicts || 0}`,'counters'); // NBSP: wrap only after "，"
      const action = make('td'); action.append(mountActions(mount,state,stale,canRetry));
      for (const [cell,label] of [[local,'本机保存'],[cloud,mount.archived ? '状态' : '云端状态'],[counters,mount.archived ? '保留数据' : '待处理']]) cell.setAttribute('data-label',label);
      row.append(name, local, cloud, counters, action); return row;
    };
    const signature = mount => JSON.stringify([{...mount,status_updated_at:mount.status_warning ? mount.status_updated_at : undefined},canRetry]);
    reconcileRows('mount-rows',mounts.filter(m => !m.archived),renderMount,signature);
    reconcileRows('archived-mount-rows',mounts.filter(m => m.archived),renderMount,signature);
  }
  function renderGlobalStatus(status) {
    const mounts = (status.mounts || []).filter(m => !m.archived), conflicts = (status.conflicts || []).length;
    const failing = mounts.some(m => m.state && (m.state.last_error || ['blocked','offline'].includes(m.state.cloud_state)));
    const waiting = mounts.some(m => m.state?.running && ((m.state.queued || 0) + (m.state.inflight || 0) > 0));
    const [text,tone] = !mounts.length ? ['尚未挂载','stopped'] : status.partial ? ['部分状态更新中','info'] : conflicts ? [`${conflicts} 个文件需要合并`,'pending'] : failing ? ['同步遇到问题','error'] : waiting ? ['有内容等待同步','pending'] : mounts.every(m => !m.state?.running) ? ['挂载未启动','stopped'] : ['同步正常','ok'];
    $('global-status').textContent = text; $('global-status').className = `chip global-status ${tone}`;
  }
  function issueAdvice(issue) {
    switch (issue.code) {
      case 'creation_needs_attention': case 'creation_unconfirmed':
        return {title:issue.code === 'creation_needs_attention' ? '创建结果需要处理' : '创建结果尚未确认',reason:'先前创建可能已经到达云端，本地正文及历史仍保留。',steps:['可重新核验先前创建的结果。','需要重新创建时，使用最新本地版本；其他位置可能仍存在先前创建的旧副本。']};
      case 'local_save_failed':
        return {title:'本机保存尚未确认',reason:'本机磁盘未能确认此次保存，当前修改仍在本机缓存中，尚不能视为持久保存。',retry:true,steps:['保留编辑器中的内容，必要时另存到其他可用磁盘；不要删除挂载或缓存。','检查磁盘剩余空间、磁盘状态，以及应用缓存目录是否可写。','恢复条件后关闭文件，保持应用运行，后台会自动重试本机保存；也可点击“重试同步”。']};
      case 'local_archive_failed':
        return {title:'本地修改等待后台整理',reason:'修改仍保留在本机，后台暂时无法生成待同步版本。',steps:['检查磁盘剩余空间，以及应用缓存目录是否可写。','保持应用运行，恢复条件后后台会自动重试；不要删除挂载或缓存。','若持续出现，请保留文件并查看应用日志。']};
      case 'remote_drift':
        return {title:'云端仍在更新，后台稍后重试',reason:'本次合并期间云端版本再次变化，已有修改保留在本机。',steps:['保持应用运行，后台将读取稳定版本后重新合并。','可以继续保存本地修改，无需手工处理合并或反复重试。']};
      case 'history_git_unavailable':
        return {title:'自动合并组件暂不可用',reason:'应用内置 Git 无法执行，当前修改已保留，同步已暂停。',steps:['保留本地文件与草稿，检查应用安装是否完整，以及安全软件是否拦截内置 Git。','修复或重新安装应用后再重试同步；无需手工安装 Git。']};
      case 'unsupported_content':
        return issue.new_document ? {title:'文档关联或格式需要处理',reason:'当前保存被识别为新文档，其中的图片、表格等结构不能直接作为新文档安全上传。',steps:['先下载本地副本，保留此次修改。','如果这是原有飞书文档，请在飞书中找到原文档，对照副本补回修改，或等待文档关联修复后同步。','如果这是全新文档，请先使用标准 Markdown 创建；不要删除现有副本。重试不会自动修复文档关联。']} : {title:'文档结构暂不支持安全写回',reason:'此次保存包含无法安全转换回飞书的结构变化，系统已停止上传。',steps:['先下载本地副本。','在飞书原文档中应用复杂表格、图片或其他结构变化；本地可编辑已支持的段落和代码块。','修正内容后重新保存；格式问题未解决前，重复重试不会生效。']};
      case 'permission_denied': case 'auth_required': case 'unauthorized': case 'identity_changed':
        return {title:'账号或权限需要检查',reason:'当前账号无法完成这项上传。',settings:true,steps:['确认使用的是拥有该文件编辑权限的账号。','在“账号与目录”中重新授权，或请目录所有者恢复编辑权限，然后重试。']};
      case 'network_or_protocol': case 'rate_limited': case 'canceled': case 'offline':
        return {title:'上传暂未完成',reason:issue.code === 'rate_limited' ? '飞书暂时限制了请求频率，后台会退避重试。' : '网络或服务响应暂未通过核验，后台会重试。',retry:true,steps:['检查网络连接；已有修改保留在本机。','恢复连接后可点击“重试同步”，仍需等待云端回读确认。']};
      case 'driver_unavailable':
        return {driver:true,title:'挂载驱动需要检查',reason:'系统挂载组件暂不可用。',steps:currentPlatform==='darwin'?['安装 macFUSE 后，在“系统设置 → 隐私与安全性”完成安装程序要求的授权。','若系统提示重启，完成重启后点击“启动挂载”；已保存内容保留。']:currentPlatform==='windows'?['从下方官方入口安装 WinFsp 运行组件。','安装完成后点击“启动挂载”，已有账号和待上传内容保留。']:['安装系统发行版提供的 FUSE3 运行组件，并确认当前用户可以访问 /dev/fuse。','然后点击“启动挂载”；已有文件和草稿保留。']};
      case 'unknown_result':
        return {title:'云端结果尚未确认',reason:'上传可能已经到达云端，但回读核验尚未完成。',retry:true,steps:['保留本地修改，不要另外创建同名云端文件。','恢复网络后重试，后台会先核验已有请求的结果。']};
      case 'empty_file_unsupported':
        return {title:'暂不支持上传空文件',reason:'当前上传接口尚未验证空文件上传能力。',steps:['先保留本地文件。','写入实际内容并保存后再同步，或在飞书中创建空文件。']};
      default:
        return {title:'同步需要处理',reason:'这项上传尚未通过安全核验，本地保存与云端完成是两个状态。',steps:['先保留或下载本地副本。','查看下方错误标识和文件路径；未确认原因前，不要删除文件或强制覆盖云端。']};
    }
  }
  function renderIssues(mounts = [], canRetry = false, canRecoverUpload = false) {
    const items = [];
    for (const mount of mounts) {
      const state = mount.state || {}, stale = Boolean(mount.status_warning);
      const issues = state.issues?.length ? state.issues : ['blocked','paused'].includes(state.cloud_state) || state.last_error ? [{code:state.last_error || state.cloud_state,name:mount.name}] : [];
      for (const issue of issues) {
        items.push({id:`${mount.id}:${issue.job_id || issue.name || issue.code}`,name:issue.name || mount.name,mount,issue,stale});
      }
    }
    reconcileRows('sync-issue-list',items,({mount,issue,stale}) => {
        const state = mount.state || {}, advice = issueAdvice(issue), card = make('article',undefined,'sync-issue');
        card.append(make('h3',issue.name || mount.name),make('strong',advice.title),make('p',advice.reason));
        const diagnostic = recovery.diagnosticView(issue);
        if (diagnostic.message || diagnostic.location) {
          const detail = make('section',undefined,'issue-diagnostic');
          detail.append(make('strong',`错误类型：${diagnostic.kind}`));
          if (diagnostic.location) detail.append(make('p',diagnostic.location));
          if (diagnostic.message) detail.append(make('p',diagnostic.message));
          if (diagnostic.text) detail.append(make('pre',diagnostic.text,'hunk-snippet'));
          card.append(detail);
        }
        if (mount.archived) card.append(make('p','此任务属于以前移除的挂载。先恢复并启动同步，处理完未同步内容后可彻底删除挂载与缓存。','quiet'));
        card.append(make('p',issue.local_saved ? '本地修改已持久保存 · 云端尚未确认此次修改' : '请保留本地文件，并核对同步状态','issue-preservation'));
        if (stale) card.append(make('p','这是上次记录，当前状态更新尚未完成；请先刷新。','quiet'));
        const steps = make('ol'); for (const step of advice.steps) steps.append(make('li',step)); card.append(steps);
        const actions = make('div',undefined,'merge-actions');
        if(advice.driver){const links={darwin:'https://macfuse.github.io/',windows:'https://winfsp.dev/rel/',linux:'https://github.com/libfuse/libfuse'};if(links[currentPlatform]){const help=make('a','官方驱动安装帮助','button small');help.href=links[currentPlatform];help.target='_blank';help.rel='noopener noreferrer';actions.append(help);}}
        if(!stale && (mount.archived || !state.running)) {
          const start=make('button',mount.archived?'恢复并启动同步':'启动挂载','button small');
          start.addEventListener('click',async()=>{start.disabled=true;try{await api(`/api/mounts/${encodeURIComponent(mount.id)}/${mount.archived?'restore_start':'start'}`,'POST',{});await loadConfig();await refresh();}catch(e){message(e.message,true);}finally{start.disabled=false;}});actions.append(start);
        }
        if (issue.create_recovery) {
          const info=issue.create_recovery;
          const reasons={location_changed:'先前创建的文件名称或位置已变化',not_found:'尚未找到先前创建的文件',multiple_candidates:'找到多个候选文件',multiple_matches:'找到多个候选文件',request_timeout:'核验请求超时',rename_unconfirmed:'云端改名结果尚未确认',content_mismatch:'候选文件内容与原提交不同',network_or_protocol:'网络或服务响应未能完成核验',permission_denied:'账号权限不足',unknown_result:'旧版本未记录首次错误原因'};
          card.append(make('p',`已核验 ${info.attempts || 0} 次 · ${reasons[info.reason] || '结果仍需核验'}`,'quiet'));
          card.append(make('p','下载副本保留原任务提交内容；重新创建使用最新本地版本。','quiet'));
          if (canRecoverUpload && issue.job_id && !mount.archived) {
            const recover=async(action,button)=>{
              if (action==='recreate' && !window.confirm(`确认使用“${issue.name || mount.name}”的最新本地版本重新创建？\n先前创建的旧副本可能已被移动到其他位置，重新创建后可能出现两份文件。原任务正文及历史会保留。`)) return;
              button.disabled=true;
              try {await api(`/api/mounts/${encodeURIComponent(mount.id)}/uploads/${encodeURIComponent(issue.job_id)}/recover`,'POST',{action,expected_generation:info.current_generation,expected_local:info.current_local,confirm_recreate:action==='recreate'});message(action==='recreate'?'已接受使用最新本地版本重新创建，仍需等待云端确认。':'已请求重新核验先前创建的结果。');await refresh();}
              catch(error){message(error.code==='version_changed'?'文件或云端状态已变化，请刷新后重新核对。':error.message,true);if(error.status===409)await refresh();}
              finally {button.disabled=stale||!state.running;}
            };
            const check=make('button','重新核验','button small');check.disabled=stale||!state.running;check.addEventListener('click',()=>recover('recheck',check));actions.append(check);
            if (info.can_recreate) {const recreate=make('button','重新创建…','button small');recreate.disabled=stale||!state.running;recreate.addEventListener('click',()=>recover('recreate',recreate));actions.append(recreate);}
          }
        }
        if (issue.can_download && issue.job_id && !stale) { const copy=make('a','下载本地副本','button small'); copy.href=`/api/mounts/${encodeURIComponent(mount.id)}/uploads/${encodeURIComponent(issue.job_id)}/local`; copy.download=issue.name?.split('/').pop() || 'local-copy'; copy.title='下载此次提交的原始内容；图片资源仍保留在挂载目录中'; actions.append(copy); }
        if (advice.settings) {const settings=make('button','检查账号与权限','button small');settings.addEventListener('click',()=>view('settings'));actions.append(settings);}
        if (advice.retry && !issue.create_recovery && canRetry && !mount.archived) {const retry=make('button','重试同步','button small');retry.disabled=stale || !state.running;retry.addEventListener('click',async()=>{retry.disabled=true;try{await api(`/api/mounts/${encodeURIComponent(mount.id)}/retry`,'POST',{});message('已请求重试，完成后需要云端确认。');await refresh();}catch(error){message(error.message,true);}finally{retry.disabled=stale||!state.running;}});actions.append(retry);}
        card.append(actions);
        const details=make('details'),summary=make('summary','诊断信息');details.append(summary,make('p',`错误标识：${issue.code || 'operation_failed'}`,'quiet'));
        if (issue.name) {
          details.append(make('p',`文件：${issue.name}`,'mount-path'));
          if (mount.mountpoint && issue.job_id) {const sep=mount.mountpoint.includes('\\') || /^[A-Za-z]:/.test(mount.mountpoint) ? '\\' : '/';const location=mount.mountpoint.replace(/[\\/]+$/,'')+sep+issue.name.split('/').join(sep);details.append(make('p',`本机位置：${location}`,'mount-path'));}
        }
        if (issue.job_id && issue.local_saved && !issue.can_download) details.append(make('p','此文件超过页面直接下载上限，请从上述本机位置复制到挂载盘之外保存副本。','quiet'));
        if (issue.job_id) details.append(make('p',`任务：${issue.job_id}`,'quiet'));
        card.append(details); return card;
    },item => JSON.stringify([item.issue,item.stale,item.mount.archived,item.mount.state?.running,canRetry,currentPlatform]));
    const count = items.length;
    $('sync-issues').hidden=activeView === 'conflicts' || !count;$('sync-issue-count').textContent=`需要处理（${count}）`;$('sync-attention').hidden=!count;$('sync-attention').textContent=`需要处理（${count}）`;
  }
  const outcomeLabels = {unreviewed:'请先打开复核',draft:'待提交',submitting:'正在提交',queued:'已接受，等待同步',resolved:'已处理，草稿保留',needs_refresh:'需要刷新并复核',failed:'提交失败，可重试',blocked:'无法自动合并，请保存副本'};
  function renderQueue() {
    const records = [...queue.records.values()];
    $('queue-empty').hidden = records.length > 0; $('conflict-queue').classList.toggle('is-empty',records.length === 0);
    const renderRecord = r => {
      const row = make('tr'), select = make('td'), checkbox = make('input'); checkbox.type = 'checkbox'; checkbox.id = `queue-select-${r.id}`; checkbox.checked = r.selected; checkbox.disabled = fileBusy(r) || (!r.selected && !queue.canSubmit(r)); checkbox.setAttribute('aria-label',`选择 ${r.name || '待合并文件'}`);
      checkbox.addEventListener('change',() => { queue.select(r.id,checkbox.checked); renderQueue(); $(checkbox.id)?.focus(); }); select.append(checkbox);
      const file = make('td'), open = make('button',r.name || '待合并文件','text-button queue-file'); open.disabled = r.status === 'resolved'; open.addEventListener('click',() => openConflict(r.id)); file.append(open);
      const draft = make('td',!r.input ? '未打开' : !r.input.text ? '非文本，保留副本' : r.seq !== r.savedSeq ? '页面草稿已保留' : r.savedSeq || r.input.has_draft ? '本机草稿已保存' : '来源已载入，尚未修改');
      const unknown = r.status === 'failed' && r.outcomeUnknown;
      const label = unknown ? (fileBusy(r) ? '结果待确认，其他文件可继续' : '结果未知，请刷新复核') : r.status === 'queued' && r.decision === 'accept_delete' ? deletedLabel(r) : outcomeLabels[r.status];
      const result = make('td'), details = make('div',undefined,'queue-details'); result.append(make('span',label,'queue-outcome ' + r.status + (unknown ? ' unknown' : '')));
      if (r.status === 'submitting' && r.slow) details.append(make('small','云端复核较慢，仍在等待结果…','queue-detail slow'));
      if (deletionDecision(r.input) && !['queued','resolved'].includes(r.status)) details.append(make('small',r.input.local_delete_pending ? '本地删除尚未完成，请打开文件单独决定。' : '云端已删除，请打开文件单独决定。','queue-detail'));
      if (r.status === 'failed') details.append(make('small',r.error || '请检查本机连接后重试。','queue-detail'));
      if (r.status === 'needs_refresh') details.append(make('small','草稿已保留；打开后刷新差异，再核对来源。','queue-detail'));
      if (r.newerDraft) details.append(make('small','本次提交后的新编辑仍保留在页面中，尚未提交。请复制保存。','queue-detail'));
      result.append(details);
      for (const [cell,label] of [[file,'文件'],[draft,'草稿'],[result,'提交结果']]) cell.setAttribute('data-label',label);
      row.append(select,file,draft,result); return row;
    };
    reconcileRows('queue-rows',records,renderRecord,r => JSON.stringify([r.pending,r.id,r.name,r.selected,r.status,r.slow,r.outcomeUnknown,r.error,r.newerDraft,r.decision,r.seq,r.savedSeq,Boolean(r.input),Boolean(r.input?.remote_deleted),Boolean(r.input?.local_delete_pending),Boolean(r.input?.text),Boolean(r.input?.has_draft),queue.canSubmit(r)]));
    const count = status => records.filter(r => r.status === status).length;
    const selected = records.filter(r => r.selected && queue.canSubmit(r)).length;
    $('submit-selected').textContent = `提交已选文件（${selected}）`;
    $('submit-selected').disabled = !selected;
    $('select-reviewed').disabled = !records.some(r => queue.canSubmit(r)); $('clear-selection').disabled = !records.some(r => r.selected);
    $('queue-summary').textContent = `共 ${records.length} 份，已选 ${selected} 份；本机已接受 ${count('queued')} 份，需要复核 ${count('needs_refresh')} 份，失败 ${count('failed')} 份。`;
  }
  function renderConflicts(conflicts, partial = false) {
    if (conflicts) queue.sync(conflicts,partial);
    const records = [...queue.records.values()];
    const count = records.filter(r => r.present && r.status !== 'queued').length;
    $('conflict-count').textContent = statusPartial ? (count ? `${count}（待更新）` : '待更新') : count; $('conflict-empty').hidden = records.length > 0;
    $('conflict-nav').hidden = records.length === 0 && !statusPartial;
    $('nav-conflict-badge').textContent = count; $('nav-conflict-badge').hidden = !count;
    // The workspace shows only a summary; the full queue lives on the conflicts page.
    $('conflict-summary').classList.toggle('attention',count > 0);
    $('conflict-summary-icon').className = 'ico ' + (count ? 'i-alert' : 'i-check');
    $('conflict-summary-title').textContent = count ? `${count} 个文件需要合并` : statusPartial ? '冲突状态待更新' : '没有待合并的冲突';
    $('conflict-summary-text').textContent = count ? '本机与云端对同一文件各有修改，逐个核对后提交。' : statusPartial ? '部分挂载的状态暂未读取完整，稍后会自动更新。' : '本机与云端内容出现不同修改时，会在这里提示。';
    $('conflict-summary-go').hidden = !count;
    reconcileRows('conflict-list',records,r => { const button = make('button',r.name || '待合并文件','conflict-item' + (current?.id === r.id ? ' selected' : '')); button.disabled = r.status === 'resolved'; button.title = r.name || ''; button.append(make('small',outcomeLabels[r.status])); button.addEventListener('click',() => openConflict(r.id)); return button; },r => JSON.stringify([r.id,r.name,r.status,current?.id === r.id]));
    renderQueue();
  }
  const preparedInputs = new Map(), desiredInputs = new Map(), inputFlights = new Map();
  let preparedBytes = 0, preparationRunning = false;
  function rememberInput(id,data,revision) {
    if (desiredInputs.get(id) !== revision || data.revision !== revision) return;
    const size = JSON.stringify(data).length * 2;
    if (size > 16 * 1024 * 1024) return;
    if (preparedInputs.has(id)) { preparedBytes -= preparedInputs.get(id).size; preparedInputs.delete(id); }
    while (preparedInputs.size >= 24 || preparedBytes + size > 16 * 1024 * 1024) { const key = preparedInputs.keys().next().value; preparedBytes -= preparedInputs.get(key).size; preparedInputs.delete(key); }
    preparedInputs.set(id,{data,size}); preparedBytes += size;
  }
  function capturedInput(id,refresh = false) {
    const revision = desiredInputs.get(id), cached = preparedInputs.get(id);
    if (!refresh && cached && cached.data.revision === revision) return Promise.resolve(cached.data);
    if (!refresh && inputFlights.has(id) && inputFlights.get(id).revision === revision) return inputFlights.get(id).promise;
    const promise = api(`/api/conflicts/${encodeURIComponent(id)}${refresh ? '?refresh=1' : ''}`);
    const flight = {revision,promise}; inputFlights.set(id,flight);
    promise.then(data => rememberInput(id,data,revision),() => {}).finally(() => { if (inputFlights.get(id) === flight) inputFlights.delete(id); });
    return promise;
  }
  function prefetchConflicts(summaries = [],partial = false) {
    const ids = new Set(summaries.map(c => c.id));
    if (!partial) for (const id of desiredInputs.keys()) if (!ids.has(id)) desiredInputs.delete(id);
    for (const c of summaries) if (c.revision) desiredInputs.set(c.id,c.revision);
    for (const [id,item] of preparedInputs) if (item.data.revision !== desiredInputs.get(id)) { preparedBytes -= item.size; preparedInputs.delete(id); }
    if (preparationRunning) return;
    const pending = summaries.filter(c => c.revision && !preparedInputs.has(c.id)).slice(0,24);
    if (!pending.length) return;
    preparationRunning = true;
    const worker = async () => { while (pending.length && !sessionLost) { const item = pending.shift(); try { await capturedInput(item.id); } catch (_) { /* Opening remains retryable; polling never changes the editor. */ } } };
    Promise.all([worker(),worker()]).finally(() => { preparationRunning = false; });
  }
  function refresh(interactive = false) {
    if (statusFlight) return statusFlight;
    statusFlight = (async () => {
      try {
        const status = await api('/api/status');
        networkFailures = 0; statusPartial = Boolean(status.partial);
        if (status.open_folder_label) { openLabel = status.open_folder_label; canOpen = status.can_open_folder === true; }
        renderMounts(status.mounts,status.can_retry); renderIssues(status.mounts,status.can_retry,status.can_recover_upload); renderConflicts(status.conflicts,status.partial); prefetchConflicts(status.conflicts,status.partial); renderGlobalStatus(status); if (current) recoveryControls();
        statusDelay = 5000;
        $('connection').textContent = status.partial ? '已连接本机服务，部分状态稍后更新' : '已连接本机服务';
        if (messageOwner === 'status_refresh') message('');
      } catch (e) {
        statusDelay = Math.min(30000,statusDelay * 2);
        if (e.status === 403) return;
        $('connection').textContent = '本机状态暂未更新，将自动重试';
        $('global-status').textContent = '本机服务未响应'; $('global-status').className = 'chip global-status error';
        // After a desktop restart this tab's port is gone; say so instead of showing raw network errors.
        networkFailures = e.code === 'network' ? networkFailures + 1 : 0;
        if (networkFailures >= 2) showSessionError('无法连接到本机 DriveBridge，它可能已退出或重启。请点击系统托盘或菜单栏中的 DriveBridge 图标，选择“打开管理页”重新打开。');
        // Background observation failures must not overwrite an editing/action notice.
        else if (interactive) message(e.code === 'network' ? e.message : '本机状态读取暂未完成，请稍后刷新。',true,'status_refresh');
      }
    })().finally(() => { statusFlight = null; });
    return statusFlight;
  }
  function scheduleStatusPoll() {
    setTimeout(async () => {
      try { if (!sessionLost) { await refresh(); if (activeView === 'settings') await discovery.load(); } }
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
  // One re-authorization action. For an already authorized account it first explains the effect,
  // then starts the recovery flow (stops the account's mounts, keeps pending uploads) when available.
  function profileRow(p, canRecover) {
    const row = make('div', undefined, 'profile-row'), title = make('div', p.name);
    title.append(make('small', `${p.brand === 'lark' ? 'Lark' : '飞书'}，${p.authorized ? '已授权' : '待授权'}`));
    const button = make('button', p.authorized ? '重新授权' : '授权', 'button small' + (p.authorized ? '' : ' primary')); row.append(title, button);
    if (!p.authorized) { button.addEventListener('click', async () => { button.disabled = true; try { await startAuth(p.id,false); } finally { button.disabled = false; } }); return row; }
    const confirm = make('div',undefined,'confirm-box inline profile-confirm'), actions = make('div',undefined,'row'), go = make('button','继续重新授权','button small primary'), cancel = make('button','取消','button small');
    confirm.hidden = true; actions.append(go,cancel);
    confirm.append(make('p','重新授权会先停止使用该账号的挂载；待上传数据、冲突和本机缓存都会保留，不会删除飞书上的文件。随后在飞书页面完成授权，并继续应用内登录；若挂载没有自动恢复，请在“文件与同步”中启动。'),actions);
    button.addEventListener('click',() => { confirm.hidden = !confirm.hidden; });
    cancel.addEventListener('click',() => { confirm.hidden = true; });
    go.addEventListener('click', async () => { go.disabled = true; try { await startAuth(p.id,canRecover); confirm.hidden = true; } finally { go.disabled = false; } });
    row.append(confirm); return row;
  }
  async function loadConfig() {
    const config = await api('/api/config'); currentPlatform=config.platform; $('macos-backend-field').hidden = config.platform !== 'darwin'; options($('profile-select'), config.profiles.filter(p => p.authorized), '先完成账号授权'); options($('remote-select'), config.remotes, '先添加共享目录'); $('profiles').replaceChildren();
    openLabel = config.open_folder_label || fileManagerLabels[config.platform] || openLabel; canOpen = config.can_open_folder === true;
    for (const p of config.profiles) $('profiles').append(profileRow(p, config.can_recover === true));
    $('add-account').open = config.profiles.length === 0;
    const previousPolicyMount = $('pull-policy-mount').value; policyMounts = (config.mounts || []).filter(m => !m.archived);
    options($('pull-policy-mount'),policyMounts,'暂无挂载');
    if (policyMounts.some(m => m.id === previousPolicyMount)) $('pull-policy-mount').value = previousPolicyMount; else $('pull-policy-mount').value = policyMounts[0]?.id || '';
    renderPullPolicy();
    for(const remote of config.remotes.filter(r=>r.account_root&&r.mount_removed)){
      const reconnect=make('button','重新挂载账号盘','button small');reconnect.addEventListener('click',async()=>{reconnect.disabled=true;try{await api('/api/config','POST',{action:'account_mount',profile_id:remote.profile_id});await loadConfig();await refresh();message('账号盘已重新挂载。');}catch(e){message(e.message,true);}finally{reconnect.disabled=false;}});$('profiles').append(reconnect);
    }
    discovery.setProfiles(config.profiles); await discovery.load();
    return config;
  }
  for (const [id, action] of [['profile-form','profile'],['remote-form','remote'],['mount-form','mount']]) $(id).addEventListener('submit', async event => { event.preventDefault(); const form = event.currentTarget; const button = form.querySelector('button'); button.disabled = true; const data = Object.fromEntries(new FormData(form)); data.action = action; try { await api('/api/config', 'POST', data); message(action === 'mount' ? '挂载配置已保存，可在“文件与同步”中启动。' : '配置已保存。'); form.reset(); await loadConfig(); await refresh(); } catch (e) { message(e.message, true); } finally { button.disabled = false; } });
  $('auth-complete').addEventListener('click', async () => { if (authAutomatic) { await authMonitor.checkNow(); return; } if (!authFlow) return; $('auth-complete').disabled = true; $('auth-state').textContent = '正在等待授权结果并核验身份…'; try { await api('/api/auth/complete','POST',{flow_id:authFlow}); $('auth-flow').hidden = true; authFlow = ''; const authorizedProfile = authProfile; authProfile = ''; message('用户授权成功，正在打开账号盘的应用登录窗口。'); await loadConfig(); if (authorizedProfile) { const started = await discovery.beginLogin(authorizedProfile); if (!started) message('用户授权成功。请在“账号盘”中检查账号状态，再打开应用登录窗口继续设置。'); } } catch (e) { $('auth-state').textContent = authErrors[e.code] || e.message; message(authErrors[e.code] || e.message, true); } finally { $('auth-complete').disabled = false; } });
  function stash(r = record()) {
    if (!r?.input?.text) return;
    try { localStorage.setItem(`draft:${r.id}`,JSON.stringify({text:r.text,generation:r.intent.generation,revision:r.intent.remote_revision})); } catch (_) { /* Keep the in-page and backend copies. */ }
  }
  function draftState(text) { $('draft-state').textContent = text; }
  function fileBusy(r = record()) { return Boolean(r?.pending > 0 || r?.status === 'submitting'); }
  function recoveryControls() {
    const r = record(), state = recovery.conflictState(current,fileBusy(r));
    const complete = ['queued','resolved'].includes(r?.status);
    $('merged-text').disabled = !state.canEdit; $('merged-text').readOnly = complete || (deletionDecision(current) && fileBusy());
    $('save-draft').disabled = !state.canSaveDraft || complete || fileBusy();
    $('resolve').hidden = deletionDecision(current);
    $('resolve').disabled = fileBusy() || !queue.canSubmit(r);
    $('resolve').title = r?.reviewRequired || current?.stale ? '请先刷新差异并重新核对来源' : '';
    $('reset-choices').hidden = deletionDecision(current);
    $('reset-choices').disabled = !state.canEdit || fileBusy() || complete;
    $('refresh-conflict').disabled = fileBusy() || complete;
    $('remote-deleted').hidden = !deletionDecision(current);
    $('deletion-title').textContent = current?.local_delete_pending ? '这是尚未上传的本地文件，之前的删除尚未完成。' : '云端文件已删除或已移出同步目录';
    $('deletion-description').textContent = current?.local_delete_pending ? '可以保留本地版本并上传，或确认删除本地文件并取消上传。文本可先在下方编辑。' : '可以保留本地版本并在云端重新创建文件，或确认跟随云端删除本地文件。文本可先在下方编辑。';
    $('restore-local').textContent = current?.local_delete_pending ? '保留本地版本并上传' : '保留本地版本并重新上传';
    $('accept-delete').textContent = current?.local_delete_pending ? '确认本地删除（取消上传）' : '跟随云端删除';
    $('restore-local').disabled = $('accept-delete').disabled = fileBusy() || !queue.canDecide(r);
    $('review-sources').hidden = !r?.reviewRequired || current?.stale || (!current?.text && !deletionDecision(current)) || complete;
    $('review-sources').disabled = fileBusy() || r?.sourcesChanged;
    $('later').disabled = fileBusy();
    $('export-copies').hidden = !current?.can_export_copies;
    $('export-copies').disabled = !state.canExport;
    const warning = state.warning || (r?.sourcesChanged ? '来源已有更新，草稿保持原样。请先刷新差异，再核对新来源。' : r?.reviewRequired ? '草稿保持原样。请对照当前来源核对已恢复的草稿，再点击“已核对新来源”允许提交。' : '');
    $('source-warning').textContent = warning; $('source-warning').hidden = !warning;
  }
  const draftIO = {save:s => api(`/api/conflicts/${encodeURIComponent(s.id)}/draft`,'PUT',{content:bytes(s.text)}),resolve:s => api(`/api/conflicts/${encodeURIComponent(s.id)}/resolve`,'POST',{conflict_id:s.id,expected_generation:s.intent.generation,expected_remote_revision:s.intent.remote_revision,...(s.action ? {action:s.action} : {}),...(s.text === undefined ? {} : {content:bytes(s.text)})})};
  async function saveDraft(id = current?.id) {
    const r = queue.records.get(id); if (!r?.input?.text || ['queued','resolved'].includes(r.status)) return true;
    if (fileBusy(r)) return false;
    clearTimeout(draftTimer); stash(r);
    try { await queue.save(id,draftIO); if (current?.id === id) draftState(r.seq === r.savedSeq ? '草稿已保存到本机' : '新编辑已保留，等待保存'); renderQueue(); return true; }
    catch(e) { if (current?.id === id) { draftState('页面草稿已保留，等待恢复连接'); message(e.message + ' 编辑仍保留在此页面，请恢复连接后保存。',true); } renderQueue(); return false; }
  }
  function scheduleSave() { const id = current?.id; clearTimeout(draftTimer); draftTimer = setTimeout(() => { if (!fileBusy(queue.records.get(id))) saveDraft(id); },650); }
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
    $('hunk-list').replaceChildren(); $('hunk-count').textContent = current.local_delete_pending ? '本地删除需要单独决定；保留时使用本地版本或下方编辑结果。' : current.remote_deleted ? '云端删除需要单独决定；保留时使用本地版本或下方编辑结果。' : `${current.hunks.length} 处差异，点击可定位三份来源`;
    if (deletionDecision(current)) return;
    for (const h of current.hunks) {
      const row = make('div',undefined,'hunk-row' + (h.id === activeHunk ? ' active' : '')); row.dataset.hunk = h.id;
      const heading = make('div',undefined,'hunk-row-heading');
      const jump = make('button',`差异 ${h.id + 1}，原版${sourceRange(h,'Base')}`,'hunk-jump');
      jump.title = '定位到三份来源的对应文本'; jump.addEventListener('click',() => jumpHunk(h.id));
      const group = make('div',undefined,'hunk-choices');
      for (const [key,label] of [['Local','采用本地'],['Remote','采用云端'],['Base','还原原版']]) {
        const button = make('button',label,'choice' + ((choices.get(h.id) || 'Local') === key ? ' active' : '')); button.disabled = manual || fileBusy() || !current.text || record()?.status === 'queued';
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
    current = r.input; current.hunks ||= []; choices = r.choices; manual = r.manual; activeHunk = null; mergeOpen = true;
    $('merge').hidden = false; view('conflicts');
    $('merge-name').textContent = r.name; $('merge-version').textContent = current.local_delete_pending ? `本地代次 ${current.generation}，尚未上传到云端。` : current.remote_deleted ? '云端文件已不在原目录，本地修改已保留。' : `本地代次 ${current.generation}，云端修订 ${current.remote_revision || '下载观测'}`;
    $('merge-version').textContent += ` · 原因：${recovery.conflictReason(current.reason)}${current.reason ? '（' + current.reason + '）' : ''}`;
    $('merge-hint').hidden = deletionDecision(current);
    $('merge-hint').textContent = current.reason === 'unsupported_content'
      ? '草稿已保留。当前修改包含暂不支持写回的结构，或无法准确定位的内容；请保留表格、图片等结构后核对正文。提交仍会进行安全检查。'
      : current.reason === 'ambiguous_mapping'
        ? '草稿已保留。部分段落重复或位置变化，暂时无法准确定位；请核对三份来源后修改结果，避免覆盖错误段落。'
        : '三份来源保持只读。在右侧编辑结果，或逐个差异选择内容。提交时会重新检查本地和云端版本。';
    renderSources(); $('merged-text').value = r.text; recoveryControls(); renderHunks();
    $('copy-paths').replaceChildren(); $('copy-paths').hidden = true;
    draftState(r.status === 'queued' ? r.decision === 'accept_delete' ? deletedLabel(r) : (r.newerDraft ? '新编辑仍在页面中，请复制保存' : '已在本机接受，等待同步') : r.seq !== r.savedSeq ? '页面草稿已保留' : current.has_draft ? '已载入本机草稿' : '草稿未修改');
    renderConflicts();
    $('merge').scrollIntoView?.({block:'nearest'});
  }
  async function openConflict(id, reload = false) {
    const requestSeq = ++openSeq;
    clearTimeout(draftTimer);
    if (record()?.seq !== record()?.savedSeq && !fileBusy()) saveDraft();
    const existing = queue.records.get(id);
    if (existing?.input && ((!reload && (!existing.revision || existing.revision === existing.input.revision)) || existing.status === 'queued')) { showConflict(existing); return; }
    try {
      const data = await capturedInput(id,reload);
      let cached; try { cached = JSON.parse(localStorage.getItem(`draft:${id}`)); } catch (_) {}
      const r = queue.load(data,cached); stash(r);
      if (requestSeq !== openSeq) return;
      showConflict(r);
      if (!data.text) message(data.local_delete_pending ? '该文件不是可编辑文本。可直接保留本地版本并上传，或确认本地删除并取消上传。' : data.remote_deleted ? '该文件不是可编辑文本。可直接保留本地版本并重新上传，或确认跟随云端删除。' : '该文件不是可编辑文本。原版和双方副本仍保留，请保存副本后处理。');
    } catch(e) { if (requestSeq === openSeq) message(e.message,true); }
  }
  $('refresh-conflict').addEventListener('click',() => { if (current && !fileBusy()) openConflict(current.id,true); });
  $('review-sources').addEventListener('click',() => { if (current && queue.review(current.id)) { stash(); recoveryControls(); renderConflicts(); draftState('保留草稿，已核对当前来源'); } });
  $('export-copies').addEventListener('click',async () => {
    if (!current || fileBusy()) return;
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
  $('reset-choices').addEventListener('click',async () => { const id = current?.id; if (!id || fileBusy() || !(await saveDraft()) || current?.id !== id || fileBusy()) return; record().manual = manual = false; record().choices = choices = new Map(); rebuild(); renderHunks(); });
  function batchChanged(r) {
    if (r) {
      if (r.status === 'queued' && !r.newerDraft) { try { localStorage.removeItem(`draft:${r.id}`); } catch (_) {} }
      else stash(r);
    }
    renderConflicts(); recoveryControls();
    if (r && r.id === current?.id) {
      draftState(r.status === 'queued' ? r.decision === 'accept_delete' ? deletedLabel(r) : r.newerDraft ? '新编辑仍在页面中，请复制保存' : '已在本机接受，等待同步' : r.status === 'submitting' ? '正在保存并提交本次草稿…' : '草稿已保留');
      // Batch replies only change controls and status; they never replace editor text.
    }
    if (current) renderHunks();
  }
  async function submitFiles(ids) {
    clearTimeout(draftTimer); stash();
    await queue.submit(draftIO,batchChanged,ids);
    message('本次提交已处理，请查看每份文件的结果。本机接受的结果仍需等待云端确认。');
    await refresh();
    const r = record(); if (r && r.seq !== r.savedSeq && r.status !== 'queued') scheduleSave();
  }
  $('resolve').addEventListener('click',() => { if (current && queue.canSubmit(record())) submitFiles([current.id]); });
  async function decideDeletion(action) {
    const r = record(); if (fileBusy() || !queue.canDecide(r)) return;
    if (action === 'accept_delete' && !window.confirm(r.input.local_delete_pending ? '确定删除此尚未上传的本地文件并取消上传？本地文件和修改将被删除。可先保存副本。' : '确定跟随云端删除此文件？本地文件和待同步的本地修改将被删除。可先保存副本。')) return;
    clearTimeout(draftTimer); stash(r);
    await queue.decide(r.id,action,draftIO,batchChanged);
    message(r.status === 'queued' ? action === 'accept_delete' ? r.input.local_delete_pending ? '已确认本地删除，上传已取消。' : '已接受云端删除。' : r.input.local_delete_pending ? '已保留本地版本，等待上传并确认云端。' : '已保留本地版本，等待重新上传并确认云端。' : '本次决定尚未接受，草稿已保留；请查看文件状态。',r.status !== 'queued');
    await refresh();
  }
  $('restore-local').addEventListener('click',() => decideDeletion('restore_local'));
  $('accept-delete').addEventListener('click',() => decideDeletion('accept_delete'));
  $('submit-selected').addEventListener('click',() => submitFiles());
  $('select-reviewed').addEventListener('click',() => { for (const r of queue.records.values()) queue.select(r.id,true); renderQueue(); });
  $('clear-selection').addEventListener('click',() => { for (const r of queue.records.values()) queue.select(r.id,false); renderQueue(); });
  $('later').addEventListener('click',async () => { if (await saveDraft()) { $('merge').hidden = true; mergeOpen = false; message('草稿已保留，可随时从列表继续处理。'); } });
  window.addEventListener('beforeunload',event => { const unsaved = [...queue.records.values()].filter(r => r.input?.text && r.seq !== r.savedSeq); for (const r of unsaved) stash(r); if (unsaved.length) { event.preventDefault(); event.returnValue = ''; } });
  async function start() {
    const fragment = new URLSearchParams(location.hash.slice(1)); const bootstrap = fragment.get('bootstrap'); history.replaceState(null,'',location.pathname + location.search);
    try { const session = bootstrap ? await api('/api/session','POST',{bootstrap}) : await api('/api/session'); csrf = session.csrf; const config = await loadConfig(); await refresh(); authMonitor.restore(); const params = new URLSearchParams(location.search), id = params.get('conflict'); if (params.get('view') === 'conflicts' || id) view('conflicts'); if (id) await openConflict(id); else if (params.get('view') !== 'conflicts' && !config.mounts.length) view('settings'); scheduleStatusPoll(); } catch (e) { showSessionError(e.code === 'network' ? '无法连接到本机 DriveBridge，它可能已退出或重启。请点击系统托盘或菜单栏中的 DriveBridge 图标，选择“打开管理页”重新打开。' : undefined); }
  }
  start();
})();
