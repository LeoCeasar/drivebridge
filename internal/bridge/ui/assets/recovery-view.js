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
  function canRetry(mount, supported) { return Boolean(supported && mount?.state?.running); }
  const api = {conflictState,canRetry};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DriveBridgeRecovery = api;
})(typeof window === 'undefined' ? globalThis : window);
