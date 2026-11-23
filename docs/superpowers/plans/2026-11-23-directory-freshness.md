# Directory Freshness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans inline. TDD each behavioral change; one fresh whole-branch reviewer at the end.

**Goal:** 修复Windows/Linux云端新增目录长期未在挂载中出现的问题，完成当前目录优先刷新、定向刷新与准确状态。
**Architecture:** overlay拥有去重优先队列与旧缓存读取；已有元数据工作线程消费优先/常规任务；store保留完整清单与退避；manager/CLI/UI提供安全定向请求和状态。
**Tech Stack:** Go、bbolt、现有VFS通知、JS/Node、GitHub原生Windows/Linux门禁。
**Spec:** docs/superpowers/specs/2026-11-23-directory-freshness-design.md

## Global Constraints

- 缓存30秒过期；单目录网络预算30秒；优先队列256项；优先burst3后常规1项。
- 目录读取不进行云端mutation，不把失败/部分分页当作空目录。
- 维护ID/token/路径身份；保留未上传本地输入与历史、已有五分钟上传节奏和服务等待下限。
- 新请求与停止均由已有worker生命周期管理；不在List内创建无生命周期后台网络协程。
- Windows/Linux共享业务实现，原生验证与交叉编译分开说明；不自动更新本机Mac或任意未指定终端。

## Review Focus

- 已有128项扫描进行时，新打开目录必须在当前请求后获得机会。
- 反复List/手动刷新不得形成请求风暴，失败后退避不能被反复点击清除。
- 排队后目录被改名/移走或token替换，旧请求不能刷新新对象。
- 队列繁忙仍应推进普通扫描，退出不能遗留在途请求或store使用者。
- 目录清单成功采用后，通知必须到VFS；上传状态不代表目录清单新鲜。

### Task 1: 优先队列、过期读取与持久等待

**Files:** 新overlay/directory_scheduler.go及tests；修改overlay/fs.go、directory_cache.go；store/directory_cache.go。
**Interfaces:** Fs.DirectoryRefreshWake() <-chan struct{}；Fs.RequestDirectoryRefresh(ctx,relative string) error；WarmDirectoriesWithNotify继续由元数据线程调用。DirectoryStatus追加Pending/Stale/LastRefreshed/Recent有界状态。Store.RecordDirectoryOnline保存失败请求次数和NotBefore。
- [ ] RED：12/11数据复现、缓存读取不阻塞、批次中插队、重复请求合并、强制请求在途尾随一次、burst公平性、队列边界、身份漂移、失败保留/退避、30秒期限、关闭。
- [ ] GREEN：实现队列与30秒单目录读取；普通批次每个目录边界检查优先队列并保留扫描游标。
- [ ] 验证：go test ./internal/bridge/overlay ./internal/bridge/store；相关race；提交任务。

### Task 2: 生命周期、定向刷新和跨端通知

**Files:** manager/native.go、worker.go、ipc.go、新directory_refresh.go/tests；cmd/feishu/commands.go；VFS通知测试。
**Interfaces:** model.DirectoryRefreshRequest{Remote string}；Manager.RefreshDirectory(ctx,mountID,remote) error；worker /refresh-directory POST；CLI feishu mount refresh NAME --path RELATIVE。请求仅接受挂载内相对目录，不启动停止中的挂载。
- [ ] RED：路径穿越/绝对路径/文件类型/未知字段拒绝；只查询选中父目录；停止拒绝新请求；唤醒indexer；真实VFS父目录通知后看到新子目录。
- [ ] GREEN：接入现有worker刷新生命期与CLI，现有全局目录刷新同时唤醒已访问目录；不混入上传重试。
- [ ] 验证：manager/CLI/VFS相关套件及race；提交任务。

### Task 3: 准确状态与管理页定向操作

**Files:** model/types.go、manager/status/native映射、ui/types.go/server.go/assets/app.js、Node与HTTP tests。
**Interfaces:** 可选DirectoryRefreshBackend；POST /api/mounts/ID/refresh-directory {remote}；DirectoryIndexStatus展示较旧/待刷/最近目录时间。
- [ ] RED：受保护接口鉴权与请求边界、中文目录请求、暂停禁用、重复点击合并；上传已确认与旧目录清单同时显示；失败保留旧清单提示。
- [ ] GREEN：在现有挂载操作加入定向刷新表单，状态仅取服务端记录，不把已缓存比例解释为实时同步完成。
- [ ] 验证：Go UI/manager + 全部Node；提交任务。

### Task 4: 平台门禁、完整回归与独立审查

**Files:** Windows/Linux现有工作流适量补充新回归，docs/testing验证记录。
- [ ] 运行完整桥接/CLI/VFS Go套件，相关race、Node及打包测试。
- [ ] 构建Windows/Linux CLI；执行原生Windows WinFsp、Linux mount2/共享套件，不能用跳过代替必需门禁。
- [ ] 新上下文审查全分支；Important/Critical一次修正，以RED/GREEN及完整套件验证，不重复广泛审查。
- [ ] 记录源提交、测试和未执行边界。发布/部署不是本轮默认动作；用户另有终端更新指示时依其授权处理。
