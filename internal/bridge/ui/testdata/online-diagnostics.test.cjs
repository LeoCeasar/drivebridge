'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const onlinePath = path.join(__dirname,'../assets/online-view.js');
const online = fs.existsSync(onlinePath) ? require(onlinePath) : {};
const id = '1d95f4b6-809d-46a5-b109-a46727c2c464';
const failure = {schema_version:1,diagnostic_id:id,operation:'create',phase:'list_parent',cause:'auth_required',outcome:'dispatched_unconfirmed',action:'wait_auth',api_code:'429',at:'2026-11-19T03:00:00Z'};
const issue = {code:'creation_unconfirmed',job_id:id,name:'secret-file.md',local_saved:true,online:{first:{...failure,cause:'request_timeout'},last:failure,history:[failure],effective_checks:0,request_failures:5,legacy_attempts:3},create_recovery:{attempts:8,reason:'network_or_protocol',current_generation:2,current_local:'private-hash',can_recreate:false}};

test('presenter keeps cause, phase, cloud outcome and old counts independent',()=>{
  assert.equal(typeof online.present,'function','online presenter is available');
  const view = online.present(issue);
  assert.match(view.phase,/原父目录/);
  assert.match(view.cause,/授权/);
  assert.match(view.firstCause,/时间|超时/);
  assert.match(view.outcome,/已派发|尚未确认/);
  assert.match(view.counts,/有效核验：0 次/);
  assert.match(view.counts,/请求失败：5 次/);
  assert.match(view.counts,/历史累计：3 次/);
  assert.match(view.api,/API 错误码：429/);
  assert.match(view.api,/HTTP 状态：未提供/);
  assert.equal(view.settings,true);
  assert.doesNotMatch(view.next,/自动/,'suggested actions alone never promise scheduling');
});

test('missing legacy cause and unknown errors do not invent history or network evidence',()=>{
  assert.equal(typeof online.present,'function');
  const legacy = online.present({create_recovery:{attempts:5,reason:'unknown_result'}});
  assert.equal(legacy.firstCause,'首次错误原因未记录');
  assert.match(legacy.counts,/历史累计：5 次/);
  assert.doesNotMatch(legacy.counts,/有效核验：5/);
  assert.doesNotMatch(legacy.firstCause,/旧版本/);
  const view = online.present({online:{last:{cause:'unknown secret',phase:'secret',outcome:'unknown'}}});
  assert.match(view.cause,/尚未识别/);
  assert.doesNotMatch(view.cause,/网络|secret/);
  assert.match(view.phase,/未记录/);
  assert.match(online.present({online:{last:{...failure,cause:'canceled'}}}).cause,/取消来源未记录/);
  assert.match(online.present({online:{last:{...failure,cause:'rate_limited',action:'wait_backoff'},not_before:'2026-11-19T05:00:00Z',paused_reason:'unclassified_error'}}).next,/2026-11-19T05:00:00.000Z/);
});

test('diagnostic export only includes whitelisted machine facts',()=>{
  assert.equal(typeof online.exportDiagnostic,'function');
  const payload = online.exportDiagnostic({...issue,name:'private-name',path:'/private/path',url:'https://secret',identity_hash:'identity-secret',diagnostic:{text:'private-content'},online:{...issue.online,cycle_id:'not-a-uuid-secret',paused_reason:'raw secret',not_before:'secret time',last:{...failure,diagnostic_id:'private-token',operation:'https://secret',phase:'__proto__',cause:'Bearer secret',api_code:'secret',action:'private-action',raw_error:'private-error',at:'secret time',cancel_origin:'private-user',retry_after_seconds:-1},history:Array(12).fill({...failure,content:'private-content'})}});
  const encoded = JSON.stringify(payload);
  assert.doesNotMatch(encoded,/private|secret|Bearer|identity|https|__proto__/);
  assert.equal(payload.job_id,id);
  assert.equal(payload.online.last.cause,'unknown');
  assert.equal(payload.online.last.phase,'unknown');
  assert.equal(payload.online.history.length,8);
  assert.equal(payload.online.first.api_code,'429');
  assert.equal(payload.online.request_failures,5);
  assert.doesNotMatch(JSON.stringify(online.exportDiagnostic({job_id:'access-token',online:{last:{...failure,api_code:'123456789012345678901',at:'2026-11-19T00:00:00Zprivate'}}})),/access-token|123456789012345678901|private/);
});

// The app runs against a small DOM at the UI boundary; its real render and click
// handlers execute, while no authenticated browser or live sync service is used.
class Element {
  constructor(tag='div') { this.tagName=tag;this.textContent='';this.children=[];this.listeners=new Map();this.hidden=false;this.value='';this.classList={toggle(){}}; }
  addEventListener(kind,fn){this.listeners.set(kind,fn);}
  append(...children){this.children.push(...children);}
  replaceChildren(...children){this.children=children;}
  contains(item){return this===item || this.children.some(child=>child.contains(item));}
  setAttribute(){}
  querySelector(){return null;}
  scrollIntoView(){}
}
function appHarness(clipboard, withOnline=true) {
  const elements=new Map();
  const element=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
  const requests=[];
  const window={DriveBridgeAuth:require('../assets/auth-view.js'),DriveBridgeDiff:{},DriveBridgeRecovery:require('../assets/recovery-view.js'),DriveBridgeBatch:require('../assets/batch-view.js'),DriveBridgeDiscovery:{create:()=>({})},addEventListener(){},confirm(){throw new Error('unexpected confirmation');}};
  if(withOnline)window.DriveBridgeOnline=online;
  const context={document:{getElementById:element,querySelectorAll:()=>[],createElement:tag=>new Element(tag)},window,navigator:{clipboard},location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},URLSearchParams,setTimeout:()=>0,clearTimeout(){},FormData:class{},fetch:async(route,options)=>{requests.push({route,options});throw new Error('unexpected request');}};
  const source=fs.readFileSync(path.join(__dirname,'../assets/app.js'),'utf8');
  vm.runInNewContext(source.replace('  start();','  globalThis.renderOnlineIssues = renderIssues;'),context);
  const render=(value=issue,capability=true,stale=false)=>context.renderOnlineIssues([{id:'mount',name:'Mount',status_warning:stale,state:{running:true,issues:[value]}}],true,capability);
  const all=root=>[root,...root.children.flatMap(all)];
  const texts=()=>all(element('sync-issue-list')).map(e=>e.textContent).join('\n');
  const button=label=>all(element('sync-issue-list')).find(e=>e.tagName==='button'&&e.textContent===label);
  return {element,render,texts,button,requests,focus:element=>{context.document.activeElement=element;}};
}

test('real issue render shows new facts and preserves creation action permission gates',()=>{
  const app=appHarness({writeText:async()=>{}});app.render();
  assert.match(app.texts(),/当前步骤：.*原父目录/);
  assert.match(app.texts(),/最近原因：.*授权/);
  assert.match(app.texts(),/首次原因：.*时间|首次原因：.*超时/);
  assert.match(app.texts(),/云端：.*尚未确认/);
  assert.doesNotMatch(app.texts(),/已核验 8 次|旧版本|后台会自动/);
  assert.equal(app.button('重新创建…'),undefined);
  assert.ok(app.button('重新核验'));
  assert.ok(app.button('检查账号与权限'));
  app.button('检查账号与权限').listeners.get('click')();
  assert.equal(app.element('settings').hidden,false);
  assert.equal(app.requests.length,0,'settings does not start authorization or navigation requests');
  app.render({...issue,job_id:id+'2'},false);
  assert.equal(app.button('重新核验'),undefined);
});

test('diagnostic copy succeeds with safe JSON and gives diagnostic-specific feedback',async()=>{
  let copied='';const app=appHarness({writeText:async text=>{copied=text;}});app.render();
  const button=app.button('复制诊断信息');assert.ok(button);
  await button.listeners.get('click')();
  assert.equal(JSON.parse(copied).online.last.cause,'auth_required');
  assert.doesNotMatch(copied,/secret-file|private-hash/);
  assert.match(app.element('message').textContent,/诊断信息已复制/);
  assert.equal(button.disabled,false);
});
for(const [name,clipboard] of [['unavailable',undefined],['rejected',{writeText:async()=>{throw new Error('secret clipboard error');}}]]) {
  test(`diagnostic copy ${name} reports failure without leaking clipboard error`,async()=>{
    const app=appHarness(clipboard);app.render();const button=app.button('复制诊断信息');assert.ok(button);
    await button.listeners.get('click')();
    assert.match(app.element('message').textContent,/无法复制诊断信息/);
    assert.doesNotMatch(app.element('message').textContent,/secret|路径/);
    assert.equal(button.disabled,false);
  });
}
test('legacy harness without online module still renders and does not infer missing history',()=>{
  const app=appHarness(undefined,false);app.render();
  assert.match(app.texts(),/首次错误原因未记录/);
  assert.match(app.texts(),/历史累计：8 次/);
  assert.doesNotMatch(app.texts(),/旧版本/);
  assert.equal(app.button('复制诊断信息'),undefined);
});

test('business observations and service failures retain precise safe causes',()=>{
  for(const [cause,label] of [['resource_missing_unconfirmed',/未确认/],['multiple_candidates',/多个候选/],['content_mismatch',/内容.*不同/],['resource_location_changed',/位置/],['parent_pending',/上级/],['history_git_unavailable',/合并组件/],['local_state_corrupt',/本机.*状态/],['user_cancel_source_unknown',/取消来源未记录/],['quota_exceeded',/额度/],['service_unavailable',/服务.*不可用/]]) {
    const input={online:{last:{...failure,cause}}};
    assert.equal(online.exportDiagnostic(input).online.last.cause,cause);
    assert.match(online.present(input).cause,label);
  }
});

test('generation and version changes survive safe export and suggest replanning or observation',()=>{
  for(const [cause,label] of [['generation_changed',/本机.*保存/],['version_changed',/版本.*变化/]]) {
    const input={online:{last:{...failure,cause,action:'replan',outcome:'not_dispatched'}}};
    assert.equal(online.exportDiagnostic(input).online.last.cause,cause);
    assert.match(online.present(input).cause,label);
    assert.match(online.present(input).next,/重新读取并规划/);
    input.online.last={...input.online.last,outcome:'dispatched_unconfirmed',action:'observe_only'};
    assert.match(online.present(input).outcome,/尚未确认/);
    assert.match(online.present(input).next,/只读核验/);
  }
});

test('new creation recovery counters keep pre-policy history separate without online metadata',()=>{
  const view=online.present({create_recovery:{policy_version:1,attempts:8,legacy_attempts:4,effective_checks:1,request_failures:3}});
  assert.match(view.counts,/有效核验：1 次/);
  assert.match(view.counts,/请求失败：3 次/);
  assert.match(view.counts,/历史累计：4 次/);
  assert.doesNotMatch(view.counts,/历史累计：8/);
});

test('cloud completion is independent of local persistence and resolved records leave active issues',()=>{
  const app=appHarness(undefined);
  app.render({...issue,local_saved:true,code:'local_archive_failed',create_recovery:undefined,online:{last:{...failure,cause:'io_error',phase:'local_commit',outcome:'completed_verified',action:'repair_local'}}});
  assert.match(app.texts(),/云端：已核实完成/);
  assert.doesNotMatch(app.texts(),/云端尚未确认/);
  app.render({...issue,online:{...issue.online,resolved:true}});
  assert.equal(app.element('sync-issue-list').children.length,0);
  assert.equal(app.element('sync-issues').hidden,true);
});

test('stale and stopped creation actions remain disabled and recreate needs explicit permission',()=>{
  const app=appHarness(undefined);
  app.render({...issue,create_recovery:{...issue.create_recovery,can_recreate:true}},true,true);
  assert.equal(app.button('重新核验').disabled,true);
  assert.equal(app.button('重新创建…').disabled,true);
});

test('legacy latest check reason stays visible separately from missing initial failure',()=>{
  const input={create_recovery:{attempts:5,reason:'permission_denied'}};
  assert.match(online.present(input).cause,/权限不足/);
  assert.equal(online.present(input).firstCause,'首次错误原因未记录');
  assert.match(online.present({create_recovery:{reason:'network_or_protocol'}}).cause,/未区分网络或协议/);
  assert.doesNotMatch(online.present({create_recovery:{reason:'private-error'}}).cause,/private/);
});


test('a capability downgrade retires creation controls even when the old row has focus',()=>{
  const app=appHarness(undefined);app.render();
  app.focus(app.button('重新核验'));
  app.render(issue,false);
  assert.equal(app.button('重新核验'),undefined);
});
