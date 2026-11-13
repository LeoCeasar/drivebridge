'use strict';
((root) => {
  function conflictState(input, busy = false) {
    const text = Boolean(input?.text);
    const stale = Boolean(input?.stale);
    const reasons = {version_changed:'原版、本地或云端内容已变化',offline:'暂时无法连接飞书',worker_unavailable:'同步服务未启动',auth_required:'账号需要重新授权',permission_denied:'当前账号权限不足',identity_changed:'账号身份需要重新核验'};
    return {
      canEdit: text,
      canSaveDraft: text,
      canResolve: text && !stale && !busy,
      canExport: Boolean(input?.can_export_copies) && !busy,
      warning: stale ? `${reasons[input.refresh_warning] || '暂时无法重新核验云端'}。当前显示本机保留的来源，可以继续编辑并保存草稿；恢复后请刷新差异，再复核提交。` : ''
    };
  }
  function diagnosticView(issue) {
    const detail = issue?.diagnostic || {};
    const start = Number.isInteger(detail.line) && detail.line > 0 ? detail.line : 0;
    const end = Number.isInteger(detail.end_line) && detail.end_line >= start ? detail.end_line : start;
    const side = {local:'本地',remote:'云端',base:'共同原版'}[detail.side] || '';
    return {kind:detail.kind || issue?.code || 'operation_failed',message:detail.message || '',location:start ? `${side}第 ${start}${end > start ? '–' + end : ''} 行` : '',text:typeof detail.text === 'string' ? detail.text : ''};
  }
  function conflictReason(reason) {
    return {both_changed:'本地与云端双方修改了相同内容',overlapping_edits:'本地与云端双方修改了相同段落',ambiguous_mapping:'重复或移动的段落无法准确定位',unsupported_content:'文档结构无法安全自动写回',merge_complexity:'内容较复杂，需人工核对',remote_deleted:'云端文件已删除或移出目录',delete_vs_pending_edit:'云端删除与本地修改需要核对',local_delete_pending:'本地删除与待上传修改需要核对'}[reason] || '版本或内容需要核对';
  }
  function canRetry(mount, supported) { return Boolean(supported && mount?.state?.running); }
  // A retry only helps when something is waiting, unverified or failed; a fully confirmed mount has nothing to retry.
  function hasRetryWork(state) {
    if (!state) return false;
    return (state.queued || 0) > 0 || (state.inflight || 0) > 0 || Boolean(state.last_error) || ['queued','awaiting_verification','blocked','offline','unknown'].includes(state.cloud_state);
  }
  function cloudChip(state, running) {
    const labels = {synced:['已确认','ok'],queued:['等待同步','pending'],uploading:['正在上传','pending'],merging:['正在合并','pending'],awaiting_verification:['等待回读核验','pending'],conflict:['需要合并','pending'],blocked:['已阻塞','error'],paused:['已暂停','stopped'],offline:['离线','error']};
    return labels[state?.cloud_state] || (running ? ['等待确认','info'] : ['挂载未启动','stopped']);
  }
  function formatBytes(value) {
    const n = Number(value);
    if (!Number.isFinite(n) || n < 0) return '';
    const units = [['GiB',1073741824],['MiB',1048576],['KiB',1024]];
    for (const [name,size] of units) if (n >= size) { const v = n / size; return `${v >= 10 || Number.isInteger(v) ? Math.round(v) : v.toFixed(1)} ${name}`; }
    return `${Math.round(n)} B`;
  }
  // Accepts the Go duration text stored in configuration, for example "168h" or "720h0m0s".
  function retentionLabel(age) {
    const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(age || '');
    if (!age || !m) return '';
    const hours = Number(m[1] || 0) + Number(m[2] || 0) / 60 + Number(m[3] || 0) / 3600;
    if (hours === 0) return '仅按容量释放';
    return hours % 24 === 0 ? `${hours / 24} 天未使用后释放` : `${Math.max(1,Math.round(hours))} 小时未使用后释放`;
  }
  const errorLabels = {request_timeout:'云端请求超时，后台稍后重试',remote_drift:'云端仍在更新，后台稍后重试',history_git_unavailable:'自动合并组件暂不可用',local_save_failed:'本机保存尚未确认',local_archive_failed:'本地修改等待后台整理',offline:'暂时无法连接飞书',auth_required:'账号需要重新授权',permission_denied:'当前账号权限不足',version_changed:'内容已被更改，需要核对',identity_changed:'账号身份需要重新核对',worker_unavailable:'同步服务暂不可用',blocked:'同步已阻塞',unsupported_content:'有文件无法自动同步',operation_failed:'上次操作未完成'};
  const api = {diagnosticView,conflictReason,conflictState,canRetry,hasRetryWork,cloudChip,formatBytes,retentionLabel,errorLabels};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DriveBridgeRecovery = api;
})(typeof window === 'undefined' ? globalThis : window);
