# Task 1 / T01 修复轮 1 复审

## 结论

- Scoped spec verdict：**Compliant**。
- Scoped quality verdict：**Approved**。
- F1 / P1：**ADDRESSED**。
- F2 / P2：**ADDRESSED**。
- 本轮没有发现修复补丁引入的新问题，T01 代码审核没有剩余阻塞项。Windows 原生平台验收仍待执行，不将源码或 cross-build 冒充原生运行证明。

本结论关闭 `task-1-review.md` 中 F1/F2，并替代该报告由这两项问题导致的 Issues / Needs fixes 结论；不扩大本次检查范围，也不替代后续原生平台 gate。

## 范围和输入

工作树：`/Users/rrong/.codex/worktrees/5b46/drivebridge`。

基线：`50bd47b886cc4d9201c51a571ba158644f96bce1`；目标/实际 HEAD：`06ecd2124c1c28e5df9be997c570b8e278c72b1e`。本轮初始 `git status --short` 无输出。

读取 `review-50bd47b88..06ecd2124.diff` 全部七个变更文件、`task-1-report.md` 新增修复章节，以及 brief 中新增的控制器路径契约修正。实际检查默认根派生、原私有根认领流程的衔接、Windows 私有组件分类、Windows 挂载结构校验边界和全部新测试。未检查其他业务模块、旧工具目录或范围外路径；未修改产品、索引或 HEAD，未派生代理。

## F1 / P1 — ADDRESSED

位置：`internal/bridge/config/paths.go:30`；覆盖测试：`internal/bridge/config/config_test.go` 中 `TestDefaultCacheDedicatedRoot`、`TestDefaultCacheSharesBrandParentWithState`。

默认 CacheDir 现统一为 `os.UserCacheDir()/DriveBridge/Cache`。在普通 Windows 用户目录中，它与 Known Folder 下的 `DriveBridge/State` 为兄弟目录，原先必然触发的 `overlaps(StateDir, CacheDir)` 冲突消失。根认领循环仍只认领 StateDir 与 CacheDir，既不会把品牌父目录中的 State 当缓存占用，也不会给品牌父目录写 ownership marker。

该变化符合本轮明确批准的路径修正；覆盖参数的递归重叠拒绝保持原逻辑，持久 VFS 仍从 StateDir 派生。

新增测试检查精确 Cache 子目录，并在真实 TempDir 中模拟 Windows 的兄弟 State/Cache 结构和既有 `validation-tools` 兄弟组件，执行 Save/Load，确认品牌父目录未被认领且其他组件内容保留。测试 helper 在写入前替换 Known Folder 返回的 state/config 位置，并用临时用户环境派生缓存路径，因此没有以原生 Known Folder 不可注入为由写入真实用户目录。

报告提供了改动前两个行为失败和改动后的成功记录；本复审未重跑这些已通过的用例。源码与测试覆盖足以关闭原发现，没有遗留该缺陷的具体疑问。

## F2 / P2 — ADDRESSED

位置：`internal/bridge/platform/paths.go:44-62`、`internal/bridge/platform/paths_windows.go:68-77`；相关边界：`internal/bridge/config/config.go:255-261`。覆盖测试：`internal/bridge/config/paths_windows_test.go`。

私有路径遍历现在对每个存在的组件调用平台分类器。Windows 使用 `GetFileAttributes` 的 `FILE_ATTRIBUTE_REPARSE_POINT` 位拒绝所有 reparse points，避免依赖 Go 将 junction 映射为 `ModeIrregular` 的模式差异；读取属性失败会带路径上下文返回。Unix 分类器继续只检查 `ModeSymlink`，行为保持一致。

配置、state/cache、私有目录创建和 ResolveAuthRoot 仍经过同一遍历，因此原先隐藏递归的 junction 祖先及 auth/identities 路径逃逸均进入拒绝分支。保守拒绝云占位等其他 reparse 类型已在代码和报告中明确，符合本轮控制器批准的边界。检查仍是预检，未宣称消除了检查/使用竞态。

Windows 挂载路径则保留盘符/绝对路径语法校验、裸盘符根规范化和词法重叠校验，不再把 mountpoint 交给私有存储 reparse 遍历。该分支符合“活跃挂载结构校验须存活、运行时占用与目标检查由 T09 负责”的责任划分，没有引入默认派发或挂载启动行为。

新 Windows 文件通过文件名平台选择生效。fixture 直接设置真实 `IO_REPARSE_TAG_MOUNT_POINT`，并用 `os.SameFile` 确认别名确实指向目标；覆盖 state/cache 祖先拒绝及无数据落入实际挂载目标、auth 与 auth/identities 逃逸拒绝、reparse mountpoint 的 Validate/Save/Load、裸盘符及盘符根语法接受。检查了缓冲区头、UTF-16 长度/偏移、临时路径、句柄关闭和 junction 清理，未发现补丁引入的具体错误。

这些用例已记录 cross-build，但未在原生 Windows 执行；本结论只确认具体源码缺口已修复且有针对性回归覆盖，不声称原生 junction/驱动行为已通过。

## 新问题、验证和限制

本轮没有新的 findings。修复没有更改 canonical model、JSON descriptor、公开 config API、身份逻辑唯一性、schema 锁内重读或原子文件发布流程。

按派发要求，没有重复包测试、race、vet、cross-build，也没有新增临时测试或运行外部服务。实际新执行的是材料/源码读取、`git rev-parse HEAD` 和 `git status --short`；报告中的通过命令属于实施者执行记录，不标成本审核新鲜实测。

继续保留原报告的 Windows Known Folder、ACL、MoveFileEx/目录持久化、活跃驱动盘符、junction 原生用例，以及 Linux 原生环境行为待验项。原生平台 gate 与本轮 F1/F2 源码修复裁决分开；没有因环境限制虚构失败，也没有用 cross-build 代替实际平台验收。
