'use strict';
const assert = require('node:assert/strict');
const {stateLabel,membershipLabels,scanTime,loginResult,create} = require('../assets/discovery-view.js');
assert.match(stateLabel('complete'),/入口分页/);
assert.match(stateLabel('partial'),/遗漏/);
assert.match(stateLabel('stale'),/旧清单/);
assert.match(stateLabel('needs_login'),/重新登录/);
assert.match(stateLabel('needs_browser'),/登录组件/);
assert.match(stateLabel('needs_runtime'),/WebView2/);
assert.match(stateLabel('needs_extension'),/应用登录窗口/);
assert.match(stateLabel('browser_disconnected'),/已有清单仍保留/);
assert.equal(membershipLabels({memberships:['personal','shared_hidden','manual']}),'个人目录、隐藏共享、手动补录');
assert.match(membershipLabels({memberships:['manual']}),/归属待核实/);
assert.equal(scanTime({last_scan:'0001-01-01T00:00:00Z'}),'尚未扫描');

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this.value = ''; this.classList = {toggle(){}}; }
  append(...items) { this.children.push(...items); if (this.tag === 'select' && !this.value && items[0]) this.value = items[0].value; }
  replaceChildren(...items) { this.children = []; if (this.tag === 'select') this.value = ''; this.append(...items); }
  addEventListener(name,fn) { this.listeners[name] = fn; }
}
const text = e => (e.textContent || '') + (e.children || []).map(text).join('');
const elements = new Map();
const document = {getElementById(id) { if (!elements.has(id)) elements.set(id,new Element(id === 'discovery-profile' ? 'select' : 'div')); return elements.get(id); },createElement(tag) { return new Element(tag); }};
const calls = [], pending = new Map();
let cachedView = {profile_id:'a',state:'stale',browser:{extension_id:'',extension_path:'',helper_available:true,connected:false},snapshot:{entries:[{name:'旧目录',memberships:['shared_hidden']}],sources:{shared_hidden:{count:1,pages:1,status:'partial'}}}};
const api = async (route,method = 'GET',body) => {
  calls.push({route,method,body});
  if (method === 'GET') return [cachedView];
  return new Promise(resolve => pending.set(route,resolve));
};
(async () => {
  const view = create({document,api});
  view.setProfiles([{id:'unauthorized',name:'待授权',authorized:false},{id:'a',name:'账号A',authorized:true},{id:'b',name:'账号B',authorized:true}]);
  assert.equal(elements.get('discovery-profile').children.length,2);
  await view.load(); await view.load();
  assert.ok(calls.every(c => c.method === 'GET'),'polling must not start sessions, scans or mounts');
  assert.match(elements.get('discovery-status').textContent,/旧清单/);
  assert.match(elements.get('discovery-login-session').textContent,/应用独立登录窗口/);
  assert.match(elements.get('discovery-login-component').textContent,/自动核验身份/);
  assert.equal(elements.get('discovery-login-result').hidden,true);
  assert.match(text(elements.get('discovery-sources').children[2]),/隐藏共享入口.*部分完成/);
  assert.match(text(elements.get('discovery-sources').children[0]),/个人目录入口/,'counts are entrance folders, not files');
  assert.equal(elements.get('discovery-entries').children.length,1);
  assert.equal(await view.beginLogin('unauthorized'),false);
  const first = view.beginLogin('a');
  assert.equal(elements.get('discovery-controls').disabled,true);
  assert.equal(await view.beginLogin('a'),false,'duplicate actions on same account are suppressed');
  const second = view.beginLogin('b');
  assert.equal(calls.filter(c => c.method === 'POST').length,2,'different accounts have independent busy state');
  pending.get('/api/discovery/a/login')({profile_id:'a',state:'login_pending'});
  pending.get('/api/discovery/b/login')({profile_id:'b',state:'login_pending'});
  assert.equal(await first,true); assert.equal(await second,true);
  assert.equal(elements.get('discovery-controls').disabled,false);
  assert.match(elements.get('discovery-feedback').textContent,/登录遇到问题/);
  cachedView = {profile_id:'a',state:'complete',browser:{connected:true,helper_available:true},mountpoint:'/Volumes/工作盘',snapshot:{complete:true,entries:[{name:'个人目录'},{name:'共享目录'}]}};
  elements.get('discovery-profile').value = 'a'; await view.load();
  assert.equal(elements.get('discovery-login-result').hidden,false);
  assert.equal(elements.get('discovery-result-title').textContent,'登录完成');
  assert.match(elements.get('discovery-feedback').textContent,/登录完成/);
  assert.match(elements.get('discovery-result-summary').textContent,/2 个去重目录入口/);
  assert.match(elements.get('discovery-result-guidance').textContent,/可以关闭登录窗口/);
  assert.match(elements.get('discovery-result-guidance').textContent,/重新扫描/);
  cachedView.state = 'partial'; await view.load();
  assert.equal(elements.get('discovery-login-result').hidden,true,'partial data cannot claim login completion');
  assert.ok(!elements.get('discovery-feedback').textContent.includes('登录完成'),'partial state cannot retain a completed-login notice');
  assert.equal(elements.get('discovery-mountpoint').textContent,'/Volumes/工作盘','partial scans preserve the existing mountpoint');
  assert.ok(!elements.get('discovery-last-scan').textContent.includes('/Volumes'),'the mount location is shown once, not repeated in the scan line');
  assert.equal(loginResult({...cachedView,state:'stale'}).visible,false);
  assert.equal(loginResult({...cachedView,state:'complete',snapshot:{complete:false}}).visible,false,'connected helper does not prove scan or identity completion');
  assert.equal(loginResult({...cachedView,state:'needs_login'}).visible,false);
  assert.equal(loginResult({...cachedView,state:'login_pending'}).visible,false);
  assert.match(loginResult({...cachedView,mountpoint:''}).mountpoint,/尚未准备好/);
  cachedView.state = 'complete'; cachedView.browser.connected = false; await view.load();
  assert.match(elements.get('discovery-login-session').textContent,/保留上次登录/);
  assert.ok(!elements.get('discovery-login-session').textContent.includes('请打开'),'a stopped helper is not proof of logout');
  cachedView.state = 'needs_runtime'; await view.load();
  assert.equal(elements.get('discovery-runtime-help').hidden,false);
  assert.match(elements.get('discovery-login-component').textContent,/安装.*WebView2/);
  cachedView.state = 'complete'; await view.load();
  assert.equal(elements.get('discovery-runtime-help').hidden,true);
  const html = require('node:fs').readFileSync(require('node:path').join(__dirname,'../assets/index.html'),'utf8');
  assert.match(html,/目录清单保存在本机，首次元数据扫描后可立即展示/);
  assert.match(html,/文件正文在打开时按需下载/);
  assert.match(html,/补录目录按核验归属展示在个人或共享目录中/);
  assert.ok(!/chrome:\/\/extensions|扩展 ID|扩展所在目录|加载已解压/.test(html),'management UI must not show retired extension setup');
  console.log('discovery states, cached polling and per-account login controls verified');
})().catch(error => { console.error(error); process.exitCode = 1; });
