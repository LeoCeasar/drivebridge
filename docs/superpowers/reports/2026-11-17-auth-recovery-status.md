# 授权恢复与挂载状态修复验证

分支：`fix/auth-recovery-status`。基线：`26cd83d6e`。

## 修复行为

- 缺少或失效的用户凭据报告需要重新授权；凭据存储不可访问报告 `credential_unavailable`；网络核验失败保留可重试原因；实际 AppID、品牌或用户不一致继续阻止同步并报告 `identity_changed`。
- 授权恢复与 reconciliation 使用原绑定的完整预期身份。预期身份只用于验证，不会让生命周期 runner 获得业务操作授权。
- 挂载状态和配置投影后端当前授权进度，按账号隔离；新管理页也能显示继续授权入口。等待确认、应用登录、启动挂载、失败与过期均提供说明。
- 活跃授权不再显示合成的旧上传错误；真实逐文件问题、队列与冲突继续保留。旧挂载错误收进历史记录。
- 等待时保留挂载保护，按钮显示等待原因。即使焦点留在继续授权链接或详情展开，过期与完成仍会更新操作。
- 静态账号标签改为已绑定账号，不把历史绑定表示为当前凭据有效。
- 保护首次启动的空挂载 JSON 数组，避免浏览器初始化访问 null.length。

## 回归与审查

首先观察原行为失败，再实施修复：缺失/存储/核验错误误报 identity；挂载等待缺少继续入口；不同账号收到错误授权进度；完成授权丢失存储原因；unknown_result 优先级；真实 auth→CLI 的身份不一致；焦点/详情保留导致控件不更新；空挂载数组。

独立审查提出两项 Important 问题：完整身份预期未传到生命周期核验、已聚焦行不刷新授权操作。均已在一次修复中补上 RED→GREEN 回归；无 Critical、Minor 或搁置判断。

## 验证结果

| 检查 | 结果 |
|---|---|
| `go test ./lib/larkcli ./backend/feishu ./cmd/feishu ./cmd/desktop ./internal/bridge/... ./vfs/... -count=1` | 27 个有测试的包通过，其余为无测试包 |
| `go test -race ./lib/larkcli ./internal/bridge/auth ./internal/bridge/manager ./internal/bridge/ui ./internal/bridge/sync -count=1` | 5 个包通过，无数据竞争 |
| 最后空数组修复后重跑完整 UI 包及 UI race | 均通过 |
| `node --test internal/bridge/ui/testdata/*.test.cjs` | 26 项通过 |
| `TestBoundIdentityMismatchPrecedesCLICredentialFailure` | status/reconcile × user/brand × missing/storage/verification，12 个真实协议子进程用例通过 |
| macOS `cmount,desktop,drivebridge_feishu` 构建 | 成功，含挂载支持 |
| 浏览器视觉检查 | 合成授权等待、过期提示、旧错误折叠、账号隔离、重新授权说明入口均已核对 |
| `git diff --check` | 通过 |

race 链接时 Apple linker 输出 LC_DYSYMTAB warning，命令均成功退出；未出现数据竞争或测试失败。

构建命令：

```sh
CGO_ENABLED=1 CGO_CFLAGS=-I/usr/local/include/fuse go build -trimpath -tags cmount,desktop,drivebridge_feishu -o build/auth-recovery/drivebridge .
```

构建 SHA256：`8218a475d75aa8bcb32306d6fd9f5e28ec777d7dc3fe12913b9bb985aeaa6e1e`。

完整日志与开发构建保存在本分支工作区的 `build/auth-recovery/`（Git 忽略）。此产物是开发程序，不是已签名、公证的完整安装包。

## 实际限制

未替换、重启本机正在运行的安装包；未改动真实账号凭据、缓存或挂载。官方 CLI 及其凭据存储没有修改。本次修复错误分类和恢复体验，不能据此认定最初令牌文件消失的底层原因已经解决；其诊断仍依赖官方 CLI 的存储与刷新记录。跨平台共享逻辑已通过上述测试，未声称完成 Windows/Linux 原生安装验证。
