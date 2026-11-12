'use strict';
(function (root, factory) {
  const view = factory();
  if (typeof module === 'object' && module.exports) module.exports = view;
  else root.DriveBridgeDiscovery = view;
})(typeof window === 'object' ? window : globalThis, function () {
  const sources = {personal:'个人目录',shared_visible:'可见共享',shared_hidden:'隐藏共享',manual:'手动补录'};
  // A source count is the number of folder entrances; files inside them are not counted.
  const entranceLabels = {personal:'个人目录入口',shared_visible:'可见共享入口',shared_hidden:'隐藏共享入口'};
  const states = {complete:'本次入口分页完成',partial:'部分完成，清单仍有遗漏',stale:'保留旧清单，等待更新',needs_login:'需要重新登录飞书',needs_runtime:'请安装 Microsoft Edge WebView2 Runtime 后重新打开应用登录窗口',needs_browser:'应用登录组件暂不可用，请重试或安装最新完整应用包',needs_extension:'需要重新打开应用登录窗口',browser_disconnected:'需要重新登录，已有清单仍保留',unsupported:'暂不支持此来源',idle:'等待扫描',login_pending:'请在应用独立登录窗口中完成飞书登录',scanning:'正在扫描入口',unconfigured:'尚未完成应用登录',disabled:'自动发现未启用',unavailable:'暂时无法读取'};
  function stateLabel(state) { return states[state] || '等待发现目录'; }
  function membershipLabels(entry) { const memberships = entry.memberships || []; const labels = memberships.map(s => sources[s] || '其他来源').join('、') || '保留的目录入口'; return memberships.some(s => ['personal','shared_visible','shared_hidden'].includes(s)) ? labels : labels + '（归属待核实，请重新扫描后重试）'; }
  function hiddenSharedOnly(entry) {
    if (entry.manual || entry.placement === 'personal' || (entry.canonical_root_token && entry.canonical_root_token !== entry.token)) return false;
    const memberships = entry.memberships || [];
    return memberships.includes('shared_hidden') && !memberships.some(source => ['personal','shared_visible','manual'].includes(source));
  }
  function scanTime(view) { const raw = view?.last_scan || view?.snapshot?.scanned_at; const date = new Date(raw); return raw && date.getUTCFullYear() > 1 && !Number.isNaN(date.getTime()) ? date.toLocaleString() : '尚未扫描'; }
  function loginResult(view) {
    const visible = !!view?.browser?.connected && view?.state === 'complete' && view?.snapshot?.complete === true;
    const count = view?.snapshot?.entries?.length || 0;
    return {visible,title:'登录完成',summary:`已核验登录身份，本次入口分页已完成；当前清单有 ${count} 个去重目录入口。`,mountpoint:view?.mountpoint ? `账号盘挂载位置：${view.mountpoint}` : '尚未准备好，请在“文件与同步”中检查挂载状态。',guidance:'可以关闭登录窗口；已启动的挂载与后台同步会继续运行。以后点击“重新扫描”可更新目录入口。'};
  }
  function create({document, api, onChanged, copy, openLabel = () => '打开', canOpen = () => false}) {
    const $ = id => document.getElementById(id);
    const make = (tag,text,className) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (className) e.className = className; return e; };
    const views = new Map(), busy = new Set(), feedback = new Map(), candidates = new Map(), origins = new Map(), pendingHidden = new Map();
    let profiles = [], supported = true, loadSequence = 0;
    const selected = () => $('discovery-profile').value;
    function notice(id,text,error = false,kind = '') { feedback.set(id,{text,error,kind}); }
    function render() {
      const id = selected(), v = views.get(id), active = busy.has(id);
      $('discovery-controls').disabled = !id || active || !supported;
      $('discovery-profile').disabled = !profiles.length;
      $('discovery-origin').value = origins.get(id) ?? v?.origin ?? '';
      $('discovery-hidden-sync').checked = pendingHidden.get(id) ?? (v?.sync_hidden_shared !== false);
      $('discovery-hidden-sync').disabled = !id || !v?.configured || active || !supported;
      const status = !supported ? '此管理入口暂不支持自动目录发现。' : !id ? '请先添加账号并完成用户授权。' : active ? '正在处理此账号的目录操作…' : v?.state === 'unsupported' ? '当前平台的应用登录功能暂不支持，请使用支持的平台版本。' : stateLabel(v?.state);
      $('discovery-status').textContent = status;
      $('discovery-status').classList.toggle('error',!!v?.error);
      $('discovery-runtime-help').hidden = v?.state !== 'needs_runtime' && v?.error !== 'needs_runtime';
      const browser = v?.browser; $('discovery-login-window').hidden = !browser;
      if (browser) { $('discovery-login-session').textContent = loginResult(v).visible ? '应用登录会话已验证并保存。' : browser.connected ? '应用登录窗口已就绪，正在等待身份核验与目录发现。' : v?.state === 'complete' && v?.snapshot?.complete ? '已保留上次登录与目录清单，后台将自动恢复会话并复核。' : '请打开应用独立登录窗口，使用已授权的飞书账号登录。'; $('discovery-login-component').textContent = v?.state === 'needs_runtime' ? '请安装 Microsoft Edge WebView2 Runtime，再点击打开应用登录窗口；已有账号和文件会保留。' : browser.helper_available ? '完成登录后将自动核验身份、发现目录并挂载；若等待较久，可展开“登录遇到问题”重新检查。' : '应用登录组件暂不可用，请安装最新完整应用包后重试。'; }
      const result = loginResult(v); $('discovery-login-result').hidden = !result.visible;
      $('discovery-result-title').textContent = result.title; $('discovery-result-summary').textContent = result.summary; $('discovery-result-guidance').textContent = result.guidance;
      $('discovery-last-scan').textContent = scanTime(v);
      const place = v?.mountpoint || '', openable = Boolean(place && v?.mount_id && canOpen());
      $('discovery-mountpoint').textContent = place || (v?.configured ? result.mountpoint : '尚未创建');
      $('discovery-copy-mountpoint').hidden = !place || typeof copy !== 'function';
      $('discovery-open-mountpoint').hidden = !openable; $('discovery-open-mountpoint').textContent = openLabel();
      const note = feedback.get(id); $('discovery-feedback').textContent = note?.text || ''; $('discovery-feedback').hidden = !note?.text; $('discovery-feedback').classList.toggle('error',!!note?.error);
      $('discovery-sources').replaceChildren();
      for (const key of ['personal','shared_visible','shared_hidden']) {
        const source = v?.snapshot?.sources?.[key], tile = make('li');
        const count = make('span',String(source?.count || 0),'src-count'); count.append(make('small','项'));
        tile.append(make('span',entranceLabels[key],'src-name'),count,make('span',`${source?.pages || 0} 页；${key === 'shared_hidden' && v?.sync_hidden_shared === false ? '已暂停同步（保留清单）' : source ? stateLabel(source.status) : '尚未扫描'}`));
        $('discovery-sources').append(tile);
      }
      const entries = v?.snapshot?.entries || [];
      const retainedAnchors = new Set(entries.filter(entry => !hiddenSharedOnly(entry)).map(entry => entry.canonical_root_token).filter(Boolean));
      $('discovery-entry-count').textContent = `${entries.length} 个去重目录入口（含保留清单）`;
      $('discovery-entries').replaceChildren(); $('discovery-empty').hidden = entries.length > 0;
      for (const entry of entries) { const row = make('li'); row.append(make('span',entry.name),make('small',membershipLabels(entry) + (v?.sync_hidden_shared === false && hiddenSharedOnly(entry) && !retainedAnchors.has(entry.token) ? '；已暂停同步' : ''))); $('discovery-entries').append(row); }
      $('discovery-candidates').replaceChildren();
      for (const item of candidates.get(id) || []) {
        const row = make('div',undefined,'profile-row'), title = make('div',item.name); title.append(make('small',item.token));
        const add = make('button','添加','button small'); add.type = 'button'; add.disabled = active || !supported;
        add.addEventListener('click',() => action('add',{input:item.token},id)); row.append(title,add); $('discovery-candidates').append(row);
      }
    }
    async function load() {
      const seq = ++loadSequence;
      try { const list = await api('/api/discovery'); if (seq !== loadSequence) return; supported = true; for (const v of list) if (!busy.has(v.profile_id)) { views.set(v.profile_id,v); if (loginResult(v).visible && feedback.get(v.profile_id)?.kind === 'login') notice(v.profile_id,'登录完成，请查看目录发现结果与账号盘挂载位置。',false,'login_complete'); else if (!loginResult(v).visible && feedback.get(v.profile_id)?.kind === 'login_complete') notice(v.profile_id,'目录发现状态已更新，请查看各来源结果。'); } render(); }
      catch (e) { if (seq !== loadSequence) return; if (e.code === 'unavailable') supported = false; else notice(selected(),e.message,true); render(); }
    }
    async function action(kind,body = {},id = selected()) {
      if (!id || busy.has(id) || !supported) return false;
      if (kind === 'hidden-sync') { if (!views.get(id)?.configured) { render(); return false; } pendingHidden.set(id,body.enabled); }
      busy.add(id); ++loadSequence; notice(id,''); render();
      try {
        const result = await api(`/api/discovery/${encodeURIComponent(id)}/${kind}`,'POST',body);
        if (kind === 'search') { candidates.set(id,result); notice(id,result.length ? `找到 ${result.length} 个候选目录，请核对后添加。` : '没有找到匹配目录。可换一个名称，或粘贴有权访问的目录链接。'); }
        else { views.set(id,result); if (kind === 'add') notice(id,'目录已核验并补录，将按归属展示在个人或共享目录中；同一资源复用已有入口。'); else if (kind === 'login') notice(id,'已请求打开应用独立登录窗口。登录后将自动核验身份、发现目录并挂载；若等待较久，可展开“登录遇到问题”重新检查。',false,'login'); else if (kind === 'hidden-sync') notice(id,result.sync_hidden_shared === false ? '已关闭隐藏共享同步；本机缓存、待上传内容和冲突保留，重新开启后继续。' : '已开启隐藏共享同步；已知目录恢复，新增入口将在后续扫描中更新。'); else if (kind === 'complete') notice(id,loginResult(result).visible ? '登录完成，请查看目录发现结果与账号盘挂载位置。' : '已处理登录确认，请查看账号状态与目录发现结果。',false,loginResult(result).visible ? 'login_complete' : ''); else notice(id,'已更新入口清单，请查看各来源状态。'); }
        if (kind === 'complete' || kind === 'add' || kind === 'hidden-sync') await onChanged?.();
        return true;
      } catch (e) { notice(id,e.message,true); return false; }
      finally { busy.delete(id); pendingHidden.delete(id); render(); if (kind !== 'search') await load(); }
    }
    function setProfiles(items) {
      const previous = selected(); profiles = items.filter(p => p.authorized); $('discovery-profile').replaceChildren();
      if (!profiles.length) { const option = make('option','先完成账号授权'); option.value = ''; $('discovery-profile').append(option); }
      for (const p of profiles) { const option = make('option',p.name); option.value = p.id; $('discovery-profile').append(option); }
      if (profiles.some(p => p.id === previous)) $('discovery-profile').value = previous;
      render();
    }
    $('discovery-profile').addEventListener('change',render);
    $('discovery-origin').addEventListener('input',() => origins.set(selected(),$('discovery-origin').value));
    $('discovery-login').addEventListener('click',() => action('login',{origin:$('discovery-origin').value.trim()}));
    $('discovery-complete').addEventListener('click',() => action('complete'));
    $('discovery-rescan').addEventListener('click',() => action('rescan'));
    $('discovery-copy-mountpoint').addEventListener('click',() => { const text = views.get(selected())?.mountpoint; if (text && typeof copy === 'function') copy(text,$('discovery-copy-mountpoint')); });
    $('discovery-open-mountpoint').addEventListener('click',async () => {
      const id = selected(), view = views.get(id); if (!view?.mount_id) return;
      try { await api(`/api/mounts/${encodeURIComponent(view.mount_id)}/open`,'POST',{}); notice(id,''); } catch (e) { notice(id,e.message,true); }
      render();
    });
    $('discovery-hidden-sync').addEventListener('change',() => action('hidden-sync',{enabled:$('discovery-hidden-sync').checked}));
    $('discovery-search-form').addEventListener('submit',event => { event.preventDefault(); action('search',{input:$('discovery-query').value.trim()}); });
    $('discovery-add-form').addEventListener('submit',event => { event.preventDefault(); action('add',{input:$('discovery-input').value.trim()}); });
    return {load,setProfiles,beginLogin:async id => { if (!profiles.some(p => p.id === id)) return false; $('discovery-profile').value = id; render(); return action('login',{origin:(origins.get(id) ?? views.get(id)?.origin ?? '').trim()},id); }};
  }
  return {stateLabel,membershipLabels,hiddenSharedOnly,scanTime,loginResult,entranceLabels,create};
});
