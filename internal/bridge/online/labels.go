package online

var operations = map[string]struct{}{
	"create": {}, "upload": {}, "update": {}, "rename": {}, "move": {}, "delete": {}, "mkdir": {}, "directory_refresh": {}, "link": {}, "clipboard": {}, "sync": {},
}

var phases = map[string]string{
	"identity_check": "核验账号身份", "authorization_check": "检查用户授权", "list_parent": "读取原父目录", "list_page": "读取目录分页", "metadata_read": "读取云端元数据", "document_read": "获取云端正文", "conversion": "转换内容", "merge_plan": "生成合并计划", "create_dispatch": "派发创建", "write_dispatch": "派发写入", "readback": "回读核验", "rename_dispatch": "派发改名", "move_dispatch": "派发移动", "delete_dispatch": "派发删除", "namespace_verify": "核验云端名称与位置", "local_commit": "提交本地记录", "link_lookup": "查询分享链接", "clipboard_write": "写入剪贴板",
}

var messages = map[string]string{
	"resource_missing_unconfirmed": "完整核验暂未找到原操作的目标，尚不能确认原创建是否完成。",
	"multiple_candidates":          "找到多个候选对象，需要核对原操作结果。",
	"content_mismatch":             "候选对象内容与原提交不同，需要复核。",
	"resource_location_changed":    "原对象的名称或所在位置已变化，需要核对。",
	"parent_pending":               "等待上级文件夹完成云端创建。",
	"history_git_unavailable":      "自动合并组件暂不可用。",
	"local_state_corrupt":          "本机恢复记录暂不可安全读取，需要检查诊断信息。",
	"auth_required":                "用户授权已失效，需要使用原账号重新授权。",
	"credential_unavailable":       "本机凭据存储暂不可访问，请检查钥匙串或凭据管理器。",
	"identity_changed":             "账号身份发生变化，需要核对原账号与绑定关系。",
	"generation_changed":           "本机已有更新的保存，需要使用最新保存重新规划。",
	"version_changed":              "操作依据的版本已变化，需要重新读取并规划。",
	"verification_unavailable":     "账号身份暂未核验成功，具体原因尚未确认。",
	"permission_denied":            "当前账号或本机目录权限不足。",
	"scope_missing":                "应用缺少所需权限，需要检查应用授权范围。",
	"quota_exceeded":               "服务可用额度不足，请检查账号或资源额度。",
	"service_unavailable":          "服务暂时不可用，请等待后重新检查。",
	"insufficient_space":           "本机存储空间不足。",
	"io_error":                     "本机读写失败。",
	"request_timeout":              "请求执行时间已用尽。",
	"canceled":                     "请求已取消；取消来源以记录为准。",
	"dns_error":                    "域名解析失败。",
	"connection_error":             "网络连接失败。",
	"transport_error":              "连接传输失败，具体网络原因尚未确认。",
	"tls_error":                    "安全连接验证失败，请检查证书与连接配置。",
	"rate_limited":                 "服务限制了请求频率，请遵守记录的等待时间。",
	"protocol_error":               "服务响应或等待字段无法安全解析，需要重新诊断。",
	"api_error_unknown":            "服务返回了尚未识别的 API 错误。",
	"unclassified_error":           "错误原因尚未识别，请使用诊断编号进一步检查。",
	"unsupported_content":          "内容无法安全转换或写回，请检查文件结构。",
}

var actions = map[string]string{
	"wait_backoff": "等待后重新检查", "wait_dependency": "等待依赖完成", "wait_auth": "使用原账号检查授权后核验", "check_permissions": "检查所需权限", "observe_only": "只读核验原操作结果", "replan": "重新读取并规划", "review_conflict": "核对身份或处理冲突", "repair_local": "检查本机存储与保存状态", "retry_copy": "重新复制", "inspect_diagnostic": "检查诊断信息",
}

func Message(cause string) string {
	if s, ok := messages[cause]; ok {
		return s
	}
	return messages["unclassified_error"]
}
func PhaseLabel(phase string) string {
	if s, ok := phases[phase]; ok {
		return s
	}
	return "步骤未记录"
}
func ActionLabel(action string) string {
	if s, ok := actions[action]; ok {
		return s
	}
	return "检查诊断信息"
}
