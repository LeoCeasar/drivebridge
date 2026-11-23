'use strict';
((root) => {
  const operations = {create:'创建',upload:'上传',update:'更新',rename:'改名',move:'移动',delete:'删除',mkdir:'创建目录',directory_refresh:'刷新目录',link:'获取链接',clipboard:'复制',sync:'同步'};
  const phases = {identity_check:'核验账号身份',authorization_check:'检查用户授权',list_parent:'读取原父目录',list_page:'读取目录分页',metadata_read:'读取云端元数据',document_read:'获取云端正文',conversion:'转换内容',merge_plan:'生成合并计划',create_dispatch:'派发创建',write_dispatch:'派发写入',readback:'回读核验',rename_dispatch:'派发改名',move_dispatch:'派发移动',delete_dispatch:'派发删除',namespace_verify:'核验云端名称与位置',local_commit:'提交本地记录',link_lookup:'查询分享链接',clipboard_write:'写入剪贴板'};
  const causes = {auth_required:'用户授权已失效，需要使用原账号重新授权',credential_unavailable:'本机凭据存储暂不可访问，请检查钥匙串或凭据管理器',identity_changed:'账号身份发生变化，需要核对原账号与绑定关系',verification_unavailable:'账号身份暂未核验成功，具体原因尚未确认',permission_denied:'当前账号或本机目录权限不足',scope_missing:'应用缺少所需权限，需要检查应用授权范围',insufficient_space:'本机存储空间不足',io_error:'本机读写失败',request_timeout:'请求执行时间已用尽',canceled:'请求已取消',dns_error:'域名解析失败',connection_error:'网络连接失败',transport_error:'连接传输失败，具体网络原因尚未确认',tls_error:'安全连接验证失败，请检查证书与连接配置',rate_limited:'服务限制了请求频率',protocol_error:'服务响应无法安全解析，需要重新诊断',api_error_unknown:'服务返回了尚未识别的 API 错误',unclassified_error:'错误原因尚未识别',unsupported_content:'内容无法安全转换或写回'};
  Object.assign(causes,{quota_exceeded:'服务可用额度不足，请检查账号或资源额度',service_unavailable:'服务暂时不可用',resource_missing_unconfirmed:'尚未找到原对象，是否缺失仍未确认',multiple_candidates:'找到多个候选文件，需要核对',content_mismatch:'候选文件内容与原提交不同',resource_location_changed:'原对象名称或位置已变化',parent_pending:'等待上级文件夹完成创建',history_git_unavailable:'自动合并组件暂不可用',local_state_corrupt:'本机恢复状态无法安全读取',user_cancel_source_unknown:'取消来源未记录'});
  Object.assign(causes,{generation_changed:'本机已有更新的保存，需要使用最新保存重新规划',version_changed:'操作依据的版本已变化，需要重新读取并规划'});
  const outcomes = {not_dispatched:'已确认未派发',dispatched_unconfirmed:'已派发，云端结果尚未确认',rejected_verified:'已核实拒绝',completed_verified:'已核实完成'};
  const actions = {wait_backoff:'等待后重新检查',wait_dependency:'等待依赖完成',wait_auth:'使用原账号检查授权后核验',check_permissions:'检查所需权限',observe_only:'只读核验原操作结果',replan:'重新读取并规划',review_conflict:'核对身份或处理冲突',repair_local:'检查本机存储与保存状态',retry_copy:'重新复制',inspect_diagnostic:'检查诊断信息'};
  const cancellations = {user_stop:'用户已停止，需要继续处理后再核验',runtime_shutdown:'应用退出或升级中断',caller_gone:'调用方请求结束',phase_deadline:'当前步骤时间已用尽',parent_deadline:'整体处理时间已用尽',unspecified_cancel:'取消来源未记录'};
  const pauses = {unclassified_error:'原因未识别，等待人工诊断',unclassified_probes_exhausted:'诊断次数已达上限',protocol_error:'响应协议需要检查',user_stop:'用户已停止',identity_changed:'账号身份需要核对',needs_attention:'需要人工处理'};
  Object.assign(pauses,{multiple_matches:'找到多个候选文件',multiple_candidates:'找到多个候选文件',content_mismatch:'候选文件内容与原提交不同',location_changed:'原对象名称或位置已变化',not_found:'完整核验仍未找到原对象',unclassified_error:'原因未识别，等待人工诊断',api_error_unknown:'API 错误含义需要检查',network_or_protocol:'请求失败原因尚未区分',request_timeout:'请求时间已用尽'});
  Object.assign(pauses,{cancel_:'取消来源未记录，需要显式继续处理',cancel_user_stop:'用户已停止，需要显式继续处理',cancel_caller_gone:'调用方已离开，需要显式继续处理',cancel_unspecified_cancel:'取消来源未记录，需要显式继续处理'});
  const legacyReasons={...causes,location_changed:'先前创建的文件名称或位置已变化',not_found:'尚未找到先前创建的文件',multiple_matches:'找到多个候选文件',rename_unconfirmed:'云端改名结果尚未确认',network_or_protocol:'核验失败原因未区分网络或协议',unknown_result:'最近核验原因未记录'};
  const own = (map,key) => typeof key === 'string' && Object.prototype.hasOwnProperty.call(map,key);
  const enumValue = (map,value) => own(map,value) ? value : 'unknown';
  const count = value => Number.isSafeInteger(value) && value >= 0 ? value : 0;
  const uuid = value => typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value) ? value.toLowerCase() : '';
  const apiCode = value => typeof value === 'string' && /^\d{1,20}$/.test(value) ? value : '';
  function timestamp(value) {
    if (typeof value !== 'string' || !/^[1-9]\d{3}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return '';
    const time = Date.parse(value); return Number.isFinite(time) ? new Date(time).toISOString() : '';
  }
  function safeFailure(input) {
    if (!input || typeof input !== 'object' || Array.isArray(input)) return null;
    const result = {schema_version:count(input.schema_version),operation:enumValue(operations,input.operation),phase:enumValue(phases,input.phase),cause:enumValue(causes,input.cause),outcome:enumValue(outcomes,input.outcome),action:enumValue(actions,input.action)};
    const id=uuid(input.diagnostic_id),at=timestamp(input.at),code=apiCode(input.api_code);
    if(id)result.diagnostic_id=id;
    if(at)result.at=at;
    if(code)result.api_code=code;
    if(input.cancel_origin)result.cancel_origin=enumValue(cancellations,input.cancel_origin);
    if(count(input.retry_after_seconds)>0)result.retry_after_seconds=count(input.retry_after_seconds);
    return result;
  }
  // A new object is built field by field. Never copy arbitrary metadata or raw
  // diagnostic strings: these may include document content, paths or secrets.
  function exportDiagnostic(issue) {
    const input=issue?.online || {},result={schema_version:1,online:{request_failures:count(input.request_failures),effective_checks:count(input.effective_checks),legacy_attempts:count(input.legacy_attempts),resolved:input.resolved===true}};
    for(const key of ['job_id','namespace_id']) {const id=uuid(issue?.[key]);if(id)result[key]=id;}
    for(const key of ['first','last']) {const failure=safeFailure(input[key]);if(failure)result.online[key]=failure;}
    result.online.history=(Array.isArray(input.history)?input.history:[]).slice(-8).map(safeFailure).filter(Boolean);
    const cycle=uuid(input.cycle_id),due=timestamp(input.not_before);
    if(cycle)result.online.cycle_id=cycle;
    if(due)result.online.not_before=due;
    if(input.paused_reason)result.online.paused_reason=enumValue(pauses,input.paused_reason);
    if(!issue?.online && issue?.create_recovery) {
      const recovery=issue.create_recovery;
      result.online.legacy_reason=enumValue(legacyReasons,recovery.reason);
      if(count(recovery.policy_version)>0) {
        result.online.effective_checks=count(recovery.effective_checks);
        result.online.request_failures=count(recovery.request_failures);
        result.online.legacy_attempts=count(recovery.legacy_attempts);
      } else result.online.legacy_attempts=count(recovery.attempts);
    }
    return result;
  }
  function causeLabel(failure) {
    if(!failure)return '错误原因未记录';
    let label=causes[failure.cause] || '错误原因尚未识别';
    if(failure.cause==='canceled')label+=`；${cancellations[failure.cancel_origin] || '取消来源未记录'}`;
    return label;
  }
  function present(issue) {
    const record=exportDiagnostic(issue).online,last=record.last,first=record.first;
    let next=actions[last?.action] || '检查诊断信息';
    if(record.paused_reason)next+=`；已暂停：${pauses[record.paused_reason] || '暂停原因未识别'}`;
    if(record.not_before)next+=`；记录的最早检查时间：${record.not_before}`;
    else if(last?.retry_after_seconds)next+=`；服务要求至少等待 ${last.retry_after_seconds} 秒（到期时间未记录）`;
    return {phase:phases[last?.phase] || '步骤未记录',cause:last?causeLabel(last):(legacyReasons[record.legacy_reason] || '错误原因未记录'),firstCause:first?causeLabel(first):'首次错误原因未记录',outcome:outcomes[last?.outcome] || '操作结果未记录',counts:`已完成有效核验：${record.effective_checks} 次；请求失败：${record.request_failures} 次${record.legacy_attempts?`；历史累计：${record.legacy_attempts} 次（未区分核验与请求失败）`:''}`,next,api:`API 错误码：${last?.api_code || '未提供'}；HTTP 状态：未提供`,id:last?.diagnostic_id || '',settings:['auth_required','credential_unavailable','identity_changed','permission_denied','scope_missing'].includes(last?.cause),resolved:record.resolved};
  }
  const api={present,exportDiagnostic};
  if(typeof module!=='undefined' && module.exports)module.exports=api;
  else root.DriveBridgeOnline=api;
})(typeof window==='undefined'?globalThis:window);
