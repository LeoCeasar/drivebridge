'use strict';
// Re-authorization, session-lost and workspace-summary states of the management page.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const auth = require('../assets/auth-view.js');
class Element {
  constructor() { this.children = []; this.hidden = true; this.listeners = {}; this.classes = new Set(); this.value = ''; this.textContent = ''; this.button = {disabled:false}; this.classList = {toggle:(name,on) => { if (on === undefined ? !this.classes.has(name) : on) this.classes.add(name); else this.classes.delete(name); }}; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  addEventListener(kind, fn) { this.listeners[kind] = fn; }
  querySelector() { return this.button; }
  setAttribute() {}
}
const elements = new Map(), element = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
const text = e => (e.textContent || '') + (e.children || []).map(text).join('');
const timers = new Map(); let timerID = 0;
const clock = {setTimeout(fn) { const id = ++timerID; timers.set(id, fn); return id; }, clearTimeout(id) { timers.delete(id); }};
const calls = []; let startReply = null, progress = {state:'opening_login'}, statusFails = false, conflicts = [];
const profiles = [{id:'authorized',name:'工作飞书',brand:'feishu',authorized:true},{id:'fresh',name:'新账号',brand:'lark',authorized:false}];
const context = {
  document:{getElementById:element,createElement:() => new Element(),querySelectorAll:() => []},
  window:{DriveBridgeAuth:{create:o => auth.create({...o,clock})},DriveBridgeDiff:{},DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeDiscovery:{create:() => ({setProfiles() {}, load: async () => {}})},addEventListener() {}},
  location:{}, setTimeout:fn => fn(),
  fetch:async (route, options) => {
    const body = options.body ? JSON.parse(options.body) : null; calls.push({route, method:options.method, body});
    if (route === '/api/config') return {ok:true,status:200,json:async () => ({platform:'windows',open_folder_label:'在资源管理器中打开',can_open_folder:true,can_recover:true,profiles,remotes:[],mounts:[]})};
    if (route === '/api/auth/start') { const r = startReply(body); return {ok:r.ok !== false,status:r.status || 200,json:async () => r.body}; }
    if (route === '/api/auth/status') return {ok:true,status:200,json:async () => ({flow_id:'f',profile_id:'authorized',...progress})};
    if (route === '/api/status') { if (statusFails) throw new TypeError('Failed to fetch'); return {ok:true,status:200,json:async () => ({mounts:[],conflicts})}; }
    throw new Error('unexpected request ' + route);
  }
};
const source = fs.readFileSync(path.join(__dirname, '../assets/app.js'), 'utf8');
vm.runInNewContext(source.replace('  start();', '  globalThis.loadConfig = loadConfig; globalThis.refresh = refresh;'), context);
(async () => {
  await context.loadConfig();
  const [authorized, fresh] = element('profiles').children;
  assert.equal(authorized.children[1].textContent, '重新授权');
  assert.equal(authorized.children.length, 3, 'one re-authorization button plus its explanation; no second text link');
  assert.equal(fresh.children[1].textContent, '授权');
  assert.ok(!element('profiles').children.some(row => text(row).includes('停止相关挂载并重新授权')), 'the two re-auth entries are merged');
  assert.equal(element('add-account').open, false, 'accounts exist, so the add form stays collapsed');

  // Re-authorizing an authorized account explains first, then uses the recovery flow once.
  const confirm = authorized.children[2];
  startReply = body => ({body:{id:'flow',url:'https://accounts.example/consent',auto_complete:false}});
  await authorized.children[1].listeners.click();
  assert.equal(confirm.hidden, false); assert.match(text(confirm), /停止使用该账号的挂载.*不会删除飞书上的文件/);
  assert.ok(!calls.some(c => c.route === '/api/auth/start'), 'nothing starts before confirmation');
  await confirm.children[1].children[0].listeners.click();
  assert.deepEqual(calls.find(c => c.route === '/api/auth/start').body, {profile_id:'authorized', recovery:true});
  assert.equal(element('auth-link').hidden, false);

  // An unauthorized account starts directly, without the recovery flag.
  await fresh.children[1].listeners.click();
  assert.deepEqual(calls.filter(c => c.route === '/api/auth/start').at(-1).body, {profile_id:'fresh'});

  // A reconciled flow has no page to open: no empty link, a plain status instead.
  startReply = () => ({body:{id:'flow2',kind:'reconciled',url:'',auto_complete:true}});
  await fresh.children[1].listeners.click(); await new Promise(setImmediate);
  assert.equal(element('auth-link').hidden, true);
  assert.match(element('auth-state').textContent, /正在自动打开应用登录窗口/, 'status polling continues through opening_login');
  assert.equal(element('auth-link').hidden, true, 'progress never reveals an empty link');
  progress = {state:'failed',error:'auth_reconcile_failed'}; const [pending] = timers.values(); await pending();
  assert.match(element('auth-state').textContent, /核验已保存的授权失败.*请重试或重启 DriveBridge/); assert.equal(element('auth-retry').hidden, false);

  // Actionable codes replace generic text; retryable failures offer a retry.
  startReply = () => ({ok:false,status:409,body:{code:'auth_identity_changed',retryable:false,message:''}});
  await fresh.children[1].listeners.click();
  assert.match(element('auth-state').textContent, /不是同一个.*未做任何更改/); assert.equal(element('auth-retry').hidden, true);
  startReply = () => ({ok:false,status:503,body:{code:'auth_reconcile_failed',retryable:true,message:''}});
  await fresh.children[1].listeners.click();
  assert.match(element('auth-state').textContent, /核验已保存的授权失败.*待上传数据已保留/); assert.equal(element('auth-retry').hidden, false);
  startReply = () => ({body:{id:'flow3',url:'https://accounts.example/again',auto_complete:false}});
  await element('auth-retry').listeners.click();
  assert.equal(element('auth-link').hidden, false); assert.equal(element('auth-retry').hidden, true);

  // The workspace shows a compact merge summary instead of the full queue.
  await context.refresh();
  assert.equal(element('conflict-summary-title').textContent, '没有待合并的冲突'); assert.equal(element('conflict-summary-go').hidden, true);
  assert.equal(element('nav-conflict-badge').hidden, true);
  conflicts = [{id:'a',name:'a.md'},{id:'b',name:'b.md'},{id:'c',name:'c.md'}]; await context.refresh();
  assert.equal(element('conflict-summary-title').textContent, '3 个文件需要合并'); assert.equal(element('conflict-summary-go').hidden, false);
  assert.equal(element('nav-conflict-badge').textContent, 3); assert.equal(element('global-status').textContent, '尚未挂载');

  // After a desktop restart this tab cannot reach the old port: a friendly full-page state, not raw errors.
  statusFails = true; await context.refresh();
  assert.equal(element('session-error').hidden, true, 'one transient failure is not session loss');
  await context.refresh();
  assert.equal(element('session-error').hidden, false); assert.equal(element('app-shell').hidden, true);
  assert.match(element('session-error-text').textContent, /系统托盘或菜单栏.*打开管理页/);
  assert.ok(!/Failed to fetch/.test(text(element('session-error'))));

  const html = fs.readFileSync(path.join(__dirname, '../assets/index.html'), 'utf8');
  assert.ok(!/任务栏|盘符(?!”)/.test(html.replace(/本机路径或 Windows 盘符/, '')), 'new wording avoids Windows-only terms');
  assert.match(html, /本机会话已失效/);
  assert.ok(!/<link[^>]+href="https?:|<script[^>]+src="https?:|@import|url\(["']?https?:/.test(html + fs.readFileSync(path.join(__dirname, '../assets/style.css'), 'utf8')), 'page works offline');
  console.log('merged re-authorization, reconciled flow, auth error codes, merge summary and session-lost state verified');
})().catch(error => { console.error(error); process.exitCode = 1; });
