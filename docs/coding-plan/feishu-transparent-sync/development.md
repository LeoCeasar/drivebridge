<!-- coding-plan: {"work_id":"feishu-transparent-sync","stage":"development","review_status":"pending","design_sha256":"dee67879ac1ca8760920f5c669f4e9b8747a6a82bd13717431cba89f757524af"} -->
# DriveBridge 飞书透明同步开发文档 v1.2

> 实施者：先读完整设计和本文。实际编码按 `superpowers:executing-plans` 逐任务执行；不得因任务写在文档中就视为已实现。每项步骤使用复选框留证据。

**设计：** [design.md](design.md)，v1.2。实际执行工作区为 `/Users/rrong/.codex/worktrees/5b46/drivebridge`，分支 `feature/feishu-transparent-sync`；不是产品运行路径。
**目标：** 交付每用户独立配置的三平台飞书挂载；Markdown 保存后透明协作同步，普通文件覆盖，冲突副本/弹窗/diff 与失败恢复。
**架构：** Rclone VFS→内部 overlay→本机持久队列→独立云端同步；远端内容只能经 VFS 空闲发布屏障进入本地视图。
**技术：** Go 1.25+、现有 Cobra/Rclone、bbolt v1.4.3、现有 go-difflib；新增 goldmark v1.8.7。初始 lark-cli 1.0.99。合并 UI 使用 Go 内嵌静态资源，无外部编辑器/CDN 依赖。
**执行边界：** 用户已在 2026-11-10 授权开始开发、多 agent；旧文档的仅文档限制已被取代。Task 0 先修订契约，独立复核 pending；随后按依赖实施，允许独立研究/复核并行，共享文件按所有者串行交接。不自动安装驱动、提权、远端写入、发布、推送或创建系统 Goal；真实账号测试需另有明确 opt-in。裁决与能力门槛见 [implementation-decisions.md](implementation-decisions.md)。

## 1. 全局约束

- R-10 已确认：尽力合并/检测/恢复；不能声称检查后写入竞态已消除，不能把 revision 当 If-Match。
- 先保证本机持久接受，再异步同步；VFS 队列清空不等于飞书已确认。
- 任何 pending、unknown、conflict 及恢复引用的 blob 都不能被缓存清理或升级删除。
- 每系统用户、profile、mount UUID 独立；身份绑定不符暂停。产品不使用实验路径、账号、folder token 或 App Secret。
- Markdown 10 MiB、语法子集、默认参数、命名和删除规则严格按设计；不支持内容不降级上传。
- 原生 Rclone 命令继续直连；只有产品 worker 创建托管 overlay。不要使原生 copy 的成功变成“只进本机队列”。
- 生命周期检查与短时本机事务在发布屏障内；所有网络调用和大 blob 准备在屏障外。
- 每次行为变更先定义能发现错误的用例、确认失败原因，再实现并验证；文档中的命令均为待执行，不伪造通过记录。

## 2. 设计与当前代码的对应

| 需求 | 当前入口/差异 | 决定 | 任务 |
|---|---|---|---|
| R-01/R-07 | `vfs/vfscache/item.go::_store` 只认一次后端写回 | overlay 本机提交与云端确认分离；持久关闭和代次 | T03/T05/T06/T07 |
| R-02/R-03 | `backend/feishu` 在线文档只读 `.url`；Markdown 原样往返有反例 | 独立 Docx codec 和侧车；新 md 规则不影响已有普通 md | T04/T05 |
| R-04/R-10 | API 已实测接受过期版本 | 三方保守合并与尽力重读；保留残余风险 | T04/T07 |
| R-05 | 已有 `--file-token`；size 初始 0 | 抽取共用执行器；size 仅从已核验版本来源或完整已知版本下载取得；保留现有覆盖机制并加核验 | T02/T07 |
| R-06 | 已有 Web GUI，但无飞书 diff | 自有内嵌本机合并页与通知；不手改上游 GUI 压缩包 | T10 |
| R-08/R-09 | 平台脚本配置各自独立 | 新统一配置和私有身份目录；worker/CLI/UI 共用接口 | T01/T08/T09 |
| R-11 | VFS 有重试；没有产品级未知结果和恢复账本 | bbolt/blob/journal、分类错误、确认后清理 | T03/T07/T11 |
| R-12 | 普通 Move/Remove 存在；虚拟 `.url` Remove 目前会删除原文档 | 修复只读删除；overlay 保持目标身份处理 temp rename | T02/T05/T06 |
| R-13 | 现有 Mac/Windows 包，无新三平台发布矩阵 | 导入迁移，统一新包与真实原生验证 | T11/T12/T13 |

## 3. 模块、类型和共享接口

### 3.1 文件所有权

| 路径 | 职责 |
|---|---|
| `lib/larkcli/{runner,errors,drive,docx}.go` | 子进程/信封/错误与官方 Drive/Docx 命令适配；不引用 internal/bridge |
| `backend/feishu/{feishu,client,metadata,naming}.go` | 原生后端，复用 runner；元数据大小、分页、只读虚拟文档 |
| `internal/bridge/model/types.go` | 设计 5.2 规范模型、稳定错误、CLI/IPC DTO |
| `internal/bridge/config/{config,paths}.go` | schema、参数验证、动态用户路径、原子配置保存 |
| `internal/bridge/store/{store,blob,jobs,conflicts,gc}.go` | 每 mount bbolt、blob 与事务；不访问网络或 VFS |
| `internal/bridge/store/{namespace,create}.go` | 本机路径/替换/tombstone 事务及云端创建身份 journal |
| `internal/bridge/markdown/{parse,render,align,merge,patch}.go` | Markdown 子集、侧车、保守对齐/三方合并、XML 序列化 |
| `internal/bridge/cloud/client.go` | runner DTO 与业务模型转换；调用文档/普通文件协议，不维护业务队列 |
| `internal/bridge/overlay/{fs,object,namespace}.go` | 内部托管 Fs、本机提交、投影及路径操作 |
| `vfs/external_update.go`、`vfs/vfscache/external_update.go` | 可显式启用的 admission、空闲失效/发布及持久关闭 |
| `vfs/managed_rename.go`、`vfs/vfscache/managed_rename.go` | 通用 Move 前的托管替换保存和源/目标缓存本机提交 |
| `internal/bridge/sync/{engine,docx,file,reconcile,poll}.go` | 云端状态机、作业逐项确认、远端刷新与核验 |
| `internal/bridge/auth/{auth,identity,coordinator}.go` | 首次应用配置、用户授权、身份绑定和状态脱敏 |
| `internal/bridge/manager/{manager,worker,ipc,status}.go` | mount 生命周期、真实 ready、认证 IPC 和综合状态 |
| `internal/bridge/platform/{process,notify,startup}_*.go` | 分平台进程、通知、登录启动；不放凭据 |
| `internal/bridge/ui/{server,session}.go`、`ui/assets/*` | 初始化页、状态、三方 diff 与版本化提交 |
| `internal/bridge/migrate/*`、`cmd/feishu/*` | 旧配置/状态迁移和 Cobra wiring |
| `packaging/drivebridge/*`、`.github/workflows/drivebridge-release.yml` | 三平台包与发布前验证 |

### 3.2 规范扩展模型

在 T01 的 `model/types.go` 定义，不能由相邻任务重新定义同名结构：

```go
type Operation struct {
    Kind, BlockID, AfterBlockID, ClientToken string; Payload json.RawMessage
    ID, Route, CapabilityID string // 可选扩展，T01 所有；T03 持久化，T02/T04/T07 消费
}
type Block struct {
    LocalID, RemoteID, ParentID, Kind, Hash string
    Start, End int; Opaque bool; RawXML []byte
}
type DocView struct { XML, Markdown []byte; Revision string; Blocks []Block }
type Hunk struct { BaseStart, BaseEnd int; Base, Local, Remote []string }
type MergeResult struct { Markdown []byte; Operations []Operation; Hunks []Hunk; ConflictReason string }
type PublishCandidate struct {
    EntryID EntryID; ExpectedGeneration uint64
    ExpectedRemote, NewRemote SnapshotID; Visible BlobID
}
type ResolveRequest struct {
    ConflictID ConflictID; ExpectedGeneration uint64
    ExpectedRemoteRevision string; Content []byte
}
type MountStatus struct {
    SchemaVersion int; MountID MountID; Running, LocalSaved bool
    VFSPending, Queued, Inflight, Conflicts int
    CloudState, LastError string
}
```

Operation.ID 是本机 journal 身份，不是服务端幂等键；ClientToken 对 docs shortcut 必须为空。Route/CapabilityID 为空时不得派发 mutation，不能猜测默认服务端能力；T04 可只产出 Kind/定位/Payload，T07 取得 T02 能力并在 RecordPlan 前补齐 ID/Route/CapabilityID。T01 拥有这些可选扩展及 JSON 兼容性，T03 负责原样保存并拒绝执行时缺路由的计划。

`Block.Start/End` 是本地 UTF-8 字节范围，左闭右开；不能用字符索引直接切 Go 字节数组。`RawXML` 和远端 ID 仅存侧车；Markdown 的 `<title>` 元数据不放进正文，标题由文件名维护。`ErrGenerationChanged`、`ErrVersionChanged`、`ErrUnsupportedContent`、`ErrUnknownResult`、`ErrIdentityChanged` 为规范可 errors.Is 的错误。

### 3.3 关键接口

以下签名为任务间契约；构造器不在业务方法中读取全局配置：

```go
// config
func DefaultPaths() (Paths, error)
func Load(path string) (Config, error)
func Save(path string, c Config) error
// Paths={ConfigFile,StateDir,CacheDir string}; Config 含 schema_version/profiles/remotes/mounts

// lib/larkcli：Request={Args []string; Stdin []byte; Dir string}
// Response={Data json.RawMessage; Warnings []string; Result string}
func (r *Runner) Run(ctx context.Context, req Request) (Response, error)
// managed业务Runner 在构造时绑定 executable/authRef/verifiedIdentity/coordinator/envPolicy；不接受从正文拼接 shell 命令
// coordinator 接口定义在 lib/larkcli（T02）：
// WithCredentialLocks(ctx, appID, userOpenID string, run func() error) error
// WithEnrollmentLock(ctx, run func() error) error
// 前者先取OS用户gate共享，再AppID/身份；后者取同gate独占，AppID未知仍被协调
// 实现归 T08；T02 离线测试注入 fake，managed 模式缺 coordinator/身份时 fail closed，不能用 no-op 锁
// managed 先 AppID 文件锁，再可选 AppID+OpenID 文件锁；覆盖全部可能自动刷新的托管子进程
// T02另定义Enrollment/AuthStream的白名单事件DTO和保留子进程句柄（Wait/CancelAndWait）；T08消费
// 仅显式注册/授权阶段可缺AppID或UserOpenID；不豁免gate/已知AppID锁
// legacy/direct 维持既有CLI配置/官方锁，不强制产品authRef，不自动迁移或共享canonical root；禁止managed降级direct

// store：所有方法作用于构造时绑定的一个 mount
func Open(dir string) (*Store, error)
func (s *Store) PutBlob(ctx context.Context, src io.Reader) (model.BlobID, error)
func (s *Store) ReadBlob(ctx context.Context, id model.BlobID) (io.ReadCloser, error)
func (s *Store) Entry(ctx context.Context, id model.EntryID) (model.Entry, error)
func (s *Store) EnsureEntry(ctx context.Context, remote string, kind model.Kind) (model.Entry, error)
func (s *Store) PrepareNamespace(ctx context.Context, op model.NamespaceOp) (model.NamespaceOp, error)
func (s *Store) SealNamespace(ctx context.Context, operationID string) (model.NamespaceOp, error)
func (s *Store) CommitNamespace(ctx context.Context, operationID string) (model.Entry, error)
func (s *Store) FinishNamespace(ctx context.Context, operationID string) error
func (s *Store) AbortNamespace(ctx context.Context, operationID string) error
func (s *Store) PendingNamespace(ctx context.Context) ([]model.NamespaceOp, error)
func (s *Store) PrepareCreate(ctx context.Context, jobID model.JobID,
    parentToken, finalName string) (model.CreateOp, error)
func (s *Store) MarkCreateUnknown(ctx context.Context, operationID string) error
func (s *Store) BindCreated(ctx context.Context, operationID, token, revision string) (model.CreateOp, error)
func (s *Store) FinishCreate(ctx context.Context, operationID string) error
func (s *Store) Stage(ctx context.Context, id model.EntryID, expected *uint64,
    src io.Reader) (model.Entry, model.Job, error)
func (s *Store) Prepare(ctx context.Context, id model.JobID) (model.Job, model.Entry, error)
func (s *Store) RecordPlan(ctx context.Context, id model.JobID, ops []model.Operation, desired model.BlobID) error
func (s *Store) RecordOperation(ctx context.Context, id model.JobID, index int, result json.RawMessage) error
func (s *Store) Confirm(ctx context.Context, id model.JobID, actual model.Snapshot) (model.PublishCandidate, error)
func (s *Store) OfferRemote(ctx context.Context, id model.EntryID, generation uint64,
    actual model.Snapshot, visible model.BlobID) (model.PublishCandidate, error)
func (s *Store) CommitPublication(ctx context.Context, c model.PublishCandidate) (model.Entry, error)
func (s *Store) SaveConflict(ctx context.Context, c model.Conflict) error
func (s *Store) Resolve(ctx context.Context, r model.ResolveRequest) (model.Entry, model.Job, error)
func (s *Store) Pending(ctx context.Context) ([]model.Job, error)

// markdown
func Decode(xml []byte, revision string) (model.DocView, error)
func Merge(baseLocal, local []byte, remote model.DocView) (model.MergeResult, error)
// Merge 返回 ConflictReason 非空时，Operations 必须为空；不能部分偷偷提交

// cloud：FileMeta={Token,Version string; Size int64; ModTime time.Time}
// Document={Token,Revision string; XML []byte; Warnings []string}
// ApplyResult={Revision,Result string; Completed []int; Warnings []string; Capabilities []OperationCapability}
// 可选 OperationCapability/ApplyResult 扩展归 T01 model；cloud 使用类型别名，lib/larkcli 的协议 DTO 归 T02 并显式转换。
// OperationCapability={ID,Kind,Route,ServerTokenField string; Verified bool; Evidence string}
// Evidence 必须标离线/真实；Verified 只批准所列操作语义，不代表 CAS 或未知结果可重试。
func (c *Client) FileMeta(ctx context.Context, token string) (FileMeta, error)
func (c *Client) FetchDoc(ctx context.Context, token, revision string) (Document, error)
func (c *Client) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (ApplyResult, error)
// 首版仅len(ops)==1；len>1拒绝unsupported_batch（len==0参数错误），签名保留以兼容将来扩展。
// T07逐次ApplyDoc→RecordOperation持久→回读/remap；cloud不得内部循环未journal的多个mutation。
// 新建、上传、下载、列举、改名、删除遵循 typed lib/larkcli Drive/Docx 方法与已读 API schema；只用固定版本支持的参数。

// overlay
func New(deps Dependencies) (fs.Fs, error)
func (f *Fs) Publish(ctx context.Context, c model.PublishCandidate) (fs.Object, error)
// Dependencies={Store *store.Store; Cloud *cloud.Client; ProfileID model.ProfileID; MountID model.MountID}

// sync：Publisher 的 commit 必须通过 vfs.TryExternalUpdate 调用 overlay.Publish
type Publisher interface { Publish(context.Context, model.PublishCandidate) error }
func (e *Engine) Run(ctx context.Context) error
func (e *Engine) SyncEntry(ctx context.Context, id model.EntryID) error

// auth：AuthFlow={ID,Kind,URL string; ExpiresAt time.Time}，浏览器不取得 device_code/飞书 token
func (a *Service) Start(ctx context.Context, profile model.ProfileID) (AuthFlow, error)
func (a *Service) Complete(ctx context.Context, flowID string) (Identity, error)
// Identity={Brand,AppID,UserOpenID string; Verified bool; Scopes []string}

// manager
func (m *Manager) Start(ctx context.Context, id model.MountID, background bool) (model.MountStatus, error)
func (m *Manager) Status(ctx context.Context, id model.MountID) (model.MountStatus, error)
func (m *Manager) Stop(ctx context.Context, id model.MountID, wait time.Duration) error
```

VFS 新 API 及 admission 规则以设计 6.2 为准。UI 使用 manager IPC，不绕过 store 操作库。普通文件/文档创建 DTO 的精确字段在 T02 契约测试中按 schema 固定并由 cloud 适配，不向上层暴露 JSON map。NamespaceOp/CreateOp 类型在设计 5.2 定义，模型所有者为 T01，事务所有者为 T03。

## 4. 重点复核场景

1. g1 正在上传时编辑器保存 g2：g1 的确认不得标记 g2 synced，也不得用合并内容替换 g2。
2. 编辑器以临时文件 rename 覆盖 `设计.md`：保留目标在线文档 Token，不能变为普通文件或删旧重建。
3. 两段文字相同或正文含 `<tag>`：映射歧义转冲突；正文按文本转义，不能作为 XML 标签丢弃。
4. 写入超时但远端已提交：重启核验，不重复创建文档或重复插入段落。
5. 用户在 diff 窗口停留时网页或本地再次修改：保留草稿并拒绝旧输入提交。

以上分别由 T03/T07、T05/T06、T04、T02/T07、T10 的明确用例覆盖，不作为无人负责的泛化 checklist。

## 5. 任务及依赖

### T01：配置、模型与用户隔离

- 需求：R-08/R-09/R-11。依赖：无。
- 文件：新增 `internal/bridge/model/types.go`、`config/{config,paths}.go`、对应 `_test.go`、`platform/paths_{darwin,windows,linux}.go`。
- 输入/输出：实现设计 5.2 与本文 3.2；`config.DefaultPaths/Load/Save`。配置保存带 schema、锁、临时文件和原子替换；Paths 可注入给测试。
- [ ] 定义测试：`TestProfileIsolation` 两个用户根/同名 profile 不共享目录；`TestUnknownSchemaReadOnly` 不覆盖未来配置；`TestConfigHasNoCredentials` 保存结果没有密钥字段；坏挂载路径和重复 UUID 被拒。
- [ ] 运行测试并记录缺失行为导致的失败。
- [ ] 按设计 5.1 JSON 固定 Config/Profile/Remote/Mount 的字段与序列化；定义当前用户身份注册表引用/验证身份 DTO 和 Operation/OperationCapability 可选扩展，禁止相同身份绑定多个 canonical auth root；实现类型、路径、身份引用、参数验证及原子保存；产品覆盖参数用 --bridge-config，Windows 使用 Known Folder/ACL，Linux 使用 XDG 规则。
- [ ] 验证：`go test ./internal/bridge/config ./internal/bridge/model ./internal/bridge/platform -count=1`；类型测试和文件权限断言通过。
- 完成：无实验账号/目录/绝对路径写入默认配置。边界：不登录、不启动 mount。

### T02：官方 CLI 契约、原生后端与准确元数据

- 需求：R-05/R-08/R-10/R-12。依赖：T01。
- 文件：新增 `lib/larkcli/{runner,errors,drive,docx}.go` 及测试；拆分/修改 `backend/feishu/{feishu,client,metadata,naming}.go` 和 `feishu_internal_test.go`；新增 `internal/bridge/cloud/client.go`。
- 输入/输出：Runner/Response、cloud.FileMeta/Document/ApplyResult；Drive DTO 固定为 list items、cursor、file_token、version、size_bytes、URL 和错误；不得把原始 map 传给 store。
- [ ] 定义 `TestRunnerErrorEnvelopeOnStderr`、`TestPartialSuccessNotConfirmed`、`TestAuthOutputRedacted`、`TestPagedJSONNoTruncation`、`TestMetadataPresenceAndCurrentVersion`、`TestVirtualURLCannotRemove`；fake process 检查 argv 含 `--as user`、注册表派生 auth root 和相对临时文件路径；检查可能自动刷新的托管普通读取/写入/探测均持共享锁，继承 CLI credential/profile/workspace 覆盖被清理且代理/证书网络变量按明确集合保留。
- [ ] 运行对应包测试确认失败原因。
- [ ] 实现 exec.CommandContext、取消、stdout/stderr 分开解析、stderr 错误信封、结构化 retryable/unknown；日志不记录 auth 原文、正文或凭据。
- [ ] 按设计 6.5 实现 preserving raw history 候选（`api GET /open-apis/drive/v1/files/{token}/history`，显式 only_tag=false，单页），保留 size/version 等字段存在性与精确整数。只有经验证当前版本来源与 history 明确关联才返回当前 FileMeta；否则验证完整的已知版本下载并绑定版本，不能证明 current 就返回 metadata_unknown。新增缺字段、错类型、负值、溢出、真实零、rename/连续覆盖及历史过滤/乱序夹具；禁止使用 shortcut 归零值、缺转空或首项当 head。缓存键含身份引用/Token/已核验 version。
- [ ] 首版只实现单页 cursor 循环；每页校验 envelope/业务 code/items/has_more/cursor，重复或缺失 cursor 失败。新增第二页业务错误且缺 data、网络断开、取消、重复 cursor 用例；失败保留旧完整 snapshot、不推断删除；成功末页后才提交完整结果。禁止 --page-all 聚合。缺时间戳用标注来源的持久首次观测值，不每次返回 time.Now。
- [ ] 固定 Docx 嵌套 DTO：fetch 的 data.document.content 与 create 的 data.document.document_id；新增 `TestDocxNestedContentAndID`、`TestDocxMissingRequiredFieldsRejected`，覆盖非空正文不读空、非空 Token 不丢失；字段 presence 区分合法空正文与缺失正文，必需 ID/版本缺失即协议错误，不移植 LarkFS 平铺解析器。
- [ ] 按设计 6.4 逐 operation 暴露路线/能力表；shortcut XML replace/delete/insert 无 client_token，本机 ID 不发送为服务端键。原生 docx 候选在参数位置、结构转换、响应和 unknown 语义逐项验证前关闭；生产能力缺失返回 unsupported_capability，typed cloud 适配不扩大能力。
- [ ] Runner 显式区分 legacy/direct 与 managed；原生 feishu: 继续既有配置和官方锁，未配置产品也可运行；不静默改用canonical root。managed（含worker复用原生适配）缺身份/协调器必须拒绝，不能降级；新增兼容性与拒绝降级测试。legacy/direct与外部CLI操作同属产品锁外生命周期风险，按设计7.1恢复。
- [ ] 验证：`go test ./lib/larkcli ./backend/feishu ./internal/bridge/cloud -count=1`；保留既有普通文件 live 测试入口，未经 opt-in 不访问飞书。
- 完成：原生命令仍保持同步上传含义；版本号不被转换成条件写保证。边界：不维护产品上传队列。

### T03：不可变快照、双 checkpoint 与作业恢复

- 需求：R-01/R-04/R-11。依赖：T01。
- 文件：新增 `internal/bridge/store/{store,blob,jobs,conflicts,gc,namespace,create}.go` 和行为测试。
- 输入/输出：本文 Store 全部接口；数据库 buckets 为 `meta/entries/jobs/conflicts/paths/recovery/namespace/createops`，meta 含 schema_version=1。只允许一个 mount worker 写库。
- [ ] 定义 `TestConfirmG1KeepsG2Pending`、`TestPrepareRebindsAcceptedCheckpoints`、`TestPublishCASRejectsNewGeneration`、`TestBlobCrashBeforeTxn`、`TestReferencedBlobNeverGC`、`TestUnknownWriteSurvivesRestart`、`TestNamespaceReplaceKeepsDestinationIdentity`、`TestCreateTokenBindingCrashRecovery`；故障注入 fsync/rename/txn 各点。
- [ ] 运行失败用例。
- [ ] 实现 blob 提交顺序、事务队列、Confirm 只确认捕获代次、双 checkpoint 及 CAS 发布；Prepare 等待同 entry 前序在途作业并重绑定最近确认的基线。
- [ ] 实现 EnsureEntry、Prepare/Seal/Commit/Finish/AbortNamespace 的路径索引/代次/tombstone 原子事务和 PendingNamespace 重放；冻结与 Job.Prepare 互斥，Seal 固定快照且拒绝新 Stage，Finish 前不投递网络；实现 PrepareCreate/MarkCreateUnknown/BindCreated/FinishCreate，输入 blob/唯一临时名必须先于网络创建持久化。
- [ ] 实现保存 conflict/草稿、引用式 GC、库备份及过期 schema 拒写；短事务不持网络调用。
- [ ] 验证：`go test -race ./internal/bridge/store -count=1`；重开库后字节和所有引用一致，未知写入没有被当作失败删除。
- 完成：本机接受成功可恢复；不能把“所有快照都存在”替代作业状态一致性。

### T04：Markdown 结构映射、保守合并和局部补丁

- 需求：R-02/R-03/R-04/R-10。依赖：T01/T03。
- 文件：新增 `internal/bridge/markdown/{parse,render,align,merge,patch}.go`、`testdata/*`、对应测试；修改 `go.mod/go.sum` 加 goldmark v1.8.7。
- 输入/输出：Decode/Merge、Block 字节范围、DocView、MergeResult；重复/重排歧义不得生成操作。使用现有 go-difflib 构造 base→local/remote 的改动区间，不引入系统 Git 依赖。
- [ ] 定义 `TestLiteralAngleTagNotLost`、`TestCodeAndBackslashRoundtrip`、`TestRepeatedBlocksConflict`、`TestDisjointEditsMerge`、`TestSameInsertionConflict`、`TestDeleteVsEditConflict`、`TestTableBothChangedConflict`、`TestOpaqueChangedBlocksUpload`、`TestUTF8SourceRanges`。
- [ ] 运行测试确认未实现行为失败。
- [ ] 实现限定语法的 AST 和原字节引用；读取 XML 时标题单独处理；输出按文本/XML 规则显式转义，不直接把官方导出的 Markdown 再送回 parser。
- [ ] 实现唯一内容哈希+相邻锚点对齐，保留 opaque RawXML；同块双方改动用保守区间三方算法，重叠/同插入点不同内容冲突；相同结果去重。
- [ ] 实现最小块操作计划，更新后失效旧块 ID 并要求回读映射；正文大小和不支持语法返回结构化冲突，Operations 为空。
- [ ] 验证：`go test -race ./internal/bridge/markdown -count=1`；用实验 XML 构造夹具，断言非目标代码/表格/高亮结构不变。
- 完成：满足设计 3.1，不扩大为任意富文本无损转换。边界：不调用云端。

### T05：托管 overlay 与文件系统名称/身份

- 需求：R-01/R-03/R-05/R-12。依赖：T01/T02/T03/T04。
- 文件：新增 `internal/bridge/overlay/{fs,object,namespace}.go`、`naming.go` 及测试。
- 输入/输出：New 返回内部 fs.Fs；Fs/Object 符合现有 fs 接口；Publish 只做 Store.CommitPublication 和新对象构造，不访问网络。
- [ ] 定义 `TestUpdateOnlyDurablyQueues`、`TestNewMDKindAndExistingRawMD`、`TestAtomicSaveKeepsDocumentToken`、`TestRenameMovesPendingGeneration`、`TestCaseFoldAndReservedNameMapping`、`TestDeleteVsPendingEditConflict`、`TestRootRemoveDenied`。
- [ ] 运行失败用例。
- [ ] 实现云端目录元数据与本机 entry 合并视图；普通文件元数据走 T02，内容只在访问时下载；在线文档被访问时获取 XML 并投影 Markdown。
- [ ] 实现设计 6.1 的 ManagedMaterializer 结构接口：未物化在线投影 Size=-1，目标需要大小时获取 XML/准确 UTF-8 字节数；readdir 名称不获取全部正文，普通对象内容读绑定不可变已核验版本。
- [ ] 实现 Stage、名称编码和持久稳定映射；新文件 `.md` 分类、普通资源类型保持、临时替换目标 entry 代次；Move 不复用原后端的删旧路径。
- [ ] 实现设计 6.3 的 Begin/Rename/Finish/AbortManagedRename 结构接口：Flush 前冻结调度、Flush 后 Seal/Commit，等待 VFS cache/node 更新后 Finish；源 inflight/unknown 返回 busy，目标已有在途作业只确认旧代次，仍保持目标 Token。
- [ ] 实现删除 tombstone/恢复引用、空目录判定及可配置临时文件过滤。
- [ ] 验证：`go test -race ./internal/bridge/overlay -count=1`；创建 Fs 不注册额外全局 backend，不改原生 feishu: 上传语义。
- 完成：已物化对象 Size 与提供字节一致；未物化投影明确为 -1，进入内核属性/打开路径前解析，不以 0 冒充空文件。边界：云端确认归 T07。

### T06：VFS 本机发布屏障和托管持久关闭

- 需求：R-01/R-07/R-11/R-12。依赖：T03/T05。
- 文件：新增 `vfs/{external_update,managed_rename}.go`、`vfs/vfscache/{external_update,managed_rename}.go` 及测试；修改 `vfs/{vfs,file,dir,read,write,read_write}.go`、`vfs/vfscache/{cache,item}.go`、`cmd/cmount/fs.go`、`cmd/mount2/node.go`；检查 `cmd/mount2/file.go` 和 `cmd/serve/nfs` 的实际反馈通道。
- 输入/输出：EnableManagedSync/TryExternalUpdate/InvalidateClean；规范错误 ErrExternalUpdateBusy/ErrManagedSyncDisabled 在 vfs 包定义。只对启用实例增加 admission 和关闭持久化。
- [ ] 定义 `TestExternalUpdateRejectsOpenReader`、`TestExternalUpdateRejectsDirtyOrUploading`、`TestOpenCannotRaceReservedUpdate`、`TestRenameMigratesHandleAdmission`、`TestManagedFlushSyncFailurePropagates`、`TestRepeatedFlushDoesNotDuplicateGeneration`、`TestManagedRenameNeverRemovesDestination`、`TestNativeModeUnchanged`；用可控 barrier，不靠 sleep 猜时序。
- [ ] 运行失败用例。
- [ ] 实现各 handle 一次性计数与关闭/Release 去重；失败的 Open 回收预留；目录 rename 迁移后代键，命名空间操作阻挡外部更新。
- [ ] 实现发布预留→原子检查/失效 clean cache→本机 commit→替换对象元数据→目录项失效→释放；禁止在检查与失效之间放行新的 Open。
- [ ] 接入 ManagedMaterializer 到托管 Getattr/Lookup/Open 的大小返回路径，覆盖 mount2/cmount；定义 `TestReaddirDoesNotMaterializeAllDocs`、`TestFirstStatAndOpenHaveExactProjectionSize`、`TestUnknownProjectionDoesNotReadAsEmpty`，解析失败传播 errno，不只修改 File.Size 的无错误返回值。
- [ ] 在 vfs.File.rename 调 operations.Move 之前拦截托管分支，包括 source o==nil；预留源/目标→BeginManagedRename 冻结调度→FlushForManagedRename 提交两边 Dirty→RenameManaged(Seal/Commit)→更新 cache/node→Finish；commit 前失败 Abort，commit 后失败保留 journal 重放；任何分支不能先调用目标 Remove。
- [ ] 用受控 barrier 定义 `TestRenameFlushCannotStartSourceUpload`，在源 Flush 完成但 Rename 尚未提交时唤醒引擎，断言源没有网络创建；补 source 已 Stage、source inflight/unknown 后重启、目标有 pending/inflight、cache 更新失败恢复测试。
- [ ] 托管 Flush/Sync 按 mutation_seq 同步数据及原子恢复元数据；排除在途 WriteAt/Truncate，重复 Flush 幂等；Release 只最终清理/调度，不作为唯一错误反馈点；Stage 未完成的 Dirty cache 能重启恢复。
- [ ] 为托管 cmount.Fsync 转发 handle.Sync，保留原生非托管路径；原生 mount2 close/Flush、fsync、WinFsp 对应通道实际注入 ENOSPC/EIO；NFS 未通过稳定写入反馈前只读。外部发布预留中不做大 blob copy 或 API，托管 rename 的本机快照复制单独受源/目标预留保护。
- [ ] 验证：`go test -race ./vfs ./vfs/vfscache/... -count=1`；原生单元和竞态测试通过。
- 完成：没有直接从 engine 写 VFS 缓存文件；持 File.mu 不调用需 Dir.mu 的方法。边界：不引入飞书/model 包到 vfs。

### T07：双向同步、覆盖核验及结果不明确的恢复

- 需求：R-04/R-05/R-07/R-10/R-11。依赖：T02/T03/T04/T05/T06。
- 文件：新增 `internal/bridge/sync/{engine,docx,file,reconcile,poll}.go`、接口测试和故障夹具。
- 输入/输出：Engine.Run/SyncEntry、Publisher；依赖 cloud、store、markdown，Publisher 由 manager 绑定到 VFS.TryExternalUpdate/overlay.Publish。
- [ ] 定义 `TestG1UploadG2SaveRebases`、`TestPartialPatchResumesWithoutDuplicate`、`TestTimeoutAfterCommitReconciles`、`TestCreateUnknownNoDuplicate`、`TestRemoteDriftThreeTimesConflicts`、`TestRemoteRefreshDefersOpenFile`、`TestPermissionAndDiskErrorsPause`、`TestOrdinaryReadbackMismatchNotConfirmed`。
- [ ] 运行失败用例。
- [ ] 实现单 entry 串行/profile 并发与限流，服从 AppID 共享凭据锁；Prepare→最新 Fetch→Merge→目标重读→校验路线能力并绑定本机 Operation.ID→RecordPlan→逐操作提交/记录→回读→Confirm→Publish。
- [ ] unknown mutation 先回读并按唯一块/内容/上下文核验；歧义转 paused/conflict，不能把本机 ID 当去重证明。补 shortcut insert/replace/delete 提交后丢响应、重复正文无法定位、部分成功后块 ID 变化、未验证原生路线拒绝派发测试；不能证明未提交时不因回读旧值自动重发。完整目录快照失败时不做删除决策。
- [ ] 每次 Prepare 与 Namespace FrozenBy 检查原子化；启动先恢复 namespace，不抢跑旧源 job；每个结构性操作完成后回读并重建剩余映射，保留本机 Operation.ID 与路线/能力证据；仅已验证原生路线才保留并发送对应服务端键。
- [ ] 实现双 checkpoint 跨新代次的 rebase；发布 busy 留 Desired，不能清新代次或强制重写打开文件。
- [ ] 实现普通文件备份/原 Token 覆盖/下载 SHA-256；无可信哈希时实际回读；unknown 先核验，非幂等创建以唯一临时名定位，歧义暂停。
- [ ] 远端拒绝零字节普通文件时标记 blocked/empty_file_unsupported，保留本地空文件；变非空后再排队，不能返回 cloud_state=synced。
- [ ] 普通文件首次上传以 CreateOp 的临时名和真实输入 blob 创建，不依赖零字节占位接口；覆盖发送前、已创建但响应丢失、取得 Token 但 BindCreated 前退出、最终改名后退出四个故障点，断言重启查找绑定而不重复创建。
- [ ] 实现活动文档 5s/目录 30s 刷新；比较 revision 而非秒级指纹；错误分类区分权限、网络、限流、版本漂移及空间不足。
- [ ] 验证：`go test -race ./internal/bridge/sync -count=1`；用故障注入模拟服务端已提交但断开响应，断言不重复操作。
- 完成：状态精确区分本机接受/云端确认；测试不声称消除了网页端竞态。

### T08：每用户首次配置、授权与身份绑定

- 需求：R-08/R-09/R-11。依赖：T01/T02/T03。
- 文件：新增 `internal/bridge/auth/{auth,identity,coordinator}.go`、测试；新增 `platform/permissions_*.go`。
- 输入/输出：Service.Start/Complete、AuthFlow、Identity；config 中只保存 credential reference 和身份摘要。
- [ ] 定义 `TestAppConfiguredButUserMissing`、`TestSameIdentitySharesAuthRootAndCoordinator`、`TestDifferentUserCannotReusePending`、`TestExpiredDeviceFlowStartsFresh`、`TestAuthSecretsNeverLogged`、`TestGrantedScopesNotAssumedRequestedOnly`。
- [ ] 运行失败用例。
- [ ] 按设计7.1.1实现非TTY `config init --new --brand` retained enrollment：T02方法流式解析stderr URL/QR/prose及最终裸stdout白名单，T08持当前OS用户gate独占（其他managed调用共享侧）；不得假设--no-wait/--json注册flag或提前知道AppID。注册无server expiry时ExpiresAt为未知，本机默认10分钟上限单独标注。
- [ ] 用户授权用 `auth login --scope <明确scopes> --json` retained streaming，不加--as；解析 device_authorization/authorization_complete 裸NDJSON事件（无ok/success），Start收到URL后返回，Complete收割同一child并核验身份/scopes/最终退出码。禁止split completion敏感device_code进入argv；T02拥有流式事件DTO/进程句柄，T08拥有flow状态。
- [ ] 新增 `TestEnrollmentUnknownAppIDBlocksOtherManagedCalls`、`TestCancelReapsBeforeUnlock`、`TestEnrollmentSavedButProbeFailsNeedsReconcile`、`TestAuthorizationCompleteMissingScopesNotSuccess`、`TestRestartReconcilesEnrollmentBeforeRetry`。取消/超时先终止并reap再解锁，保留可能已保存的enrollment状态；不假设远端未创建，不删共享凭据。重启核验残留状态，内存流程不复用；只有成功退出、身份验证和所需scopes齐全才启用同步。
- [ ] 实现 verified user 及身份摘要校验、临时登录 root→canonical 身份 root 的引用归并；相同 AppID+OpenID 复用 root/协调器，不同用户 root 分开，brand 不分官方键锁。产品只存引用/已验证身份；官方命令管理秘密，不自行复制 token 或 App Secret。
- [ ] 实现 T02 coordinator 接口：跨 worker 的 OS用户enrollment gate→AppID→AppID+OpenID 文件锁覆盖所有可刷新 child invocation；AppSecret/TAT 的 AppID 写入同样串行。引用解除与 CLI logout 分开；logout/重授权/应用密钥变更检查所有受影响引用/挂载/作业。
- [ ] 新增同 AppID 两用户、同身份多 profile、并行模拟过期刷新、共享 AppSecret/TAT 写、pending 引用拒绝 logout、继承他人凭据环境、brand 键碰撞和外部 CLI 撤销/换账号恢复测试。外部 CLI 不持产品锁，异常必须暂停旧队列并显式恢复；离线模拟不证明真实 token 旋转安全。只展示授权页面/实际 scopes，不打印密钥。
- [ ] 验证：`go test -race ./internal/bridge/auth ./internal/bridge/config -count=1`；真实新账号授权由用户在浏览器完成，不用测试脚本点击同意权限。
- 完成：应用配置/授权完成事件不能独立当作成功；最终退出码与保存后核验一致才可用，应用配置成功不能误报用户已登录；官方 CLI 管凭据，产品不自行复制 token。

### T09：worker、跨平台管理与产品 CLI

- 需求：R-01/R-07/R-08/R-09/R-11。依赖：T05/T06/T07/T08。
- 文件：新增 `internal/bridge/manager/{manager,worker,ipc,status}.go`、`platform/{process,startup}_*.go`、`cmd/feishu/{feishu,doctor,account,remote,mount,upload,conflict,cache,logs,completion}.go`；修改 `cmd/all/all.go`。GUI Cobra 入口由 T10 创建。
- 输入/输出：Manager.Start/Status/Stop；设计 7.2 命令/退出码/JSON。worker 绑定一个 mount 的库及身份，构造内部 overlay 和 mountlib.NewMountPoint，Daemon=false。
- [ ] 定义 `TestReadyRequiresMountAndIPC`、`TestStopTimeoutKeepsWorkerAndPending`、`TestPIDReuseRejected`、`TestMountWorkerIsolation`、`TestCLIJSONNoLogPollution`、`TestCacheCleanSkipsReferencedData`；stub mounter 证明流程，另保留真实驱动测试。
- [ ] 运行失败用例。
- [ ] 实现平台派生路径、运行锁、实例 UUID、loopback 认证 IPC、后台进程和登录启动；缺驱动/错误编译标签由 doctor 准确报错，不自行 sudo。
- [ ] 提供每用户 config/auth 服务与 worker IPC 调用，并实现 enable/disable-autostart；无 worker 时配置/登录服务仍可构造，静态 UI 和 feishu gui 入口由 T10 挂接；Windows 用户交互会话启动，不使用 SYSTEM/session 0。
- [ ] 实现 composite 状态与安全停止：检查打开 writer、VFS pending、产品 queued/inflight/unknown/conflict；WaitForWriters 的 void 返回不能当作 drain 成功。
- [ ] 实现 Cobra wiring、补全、显式 cache clean 和脱敏日志；profile logout/remote remove 遇引用或待上传返回 6；logout 通过 T08 对全部共享凭据引用检查，不直接运行 CLI logout。全部 CLI child 包括 doctor 中会访问凭据的探测均走 T02 环境/协调入口。
- [ ] 验证：`go test -race ./cmd/feishu ./internal/bridge/manager ./internal/bridge/platform -count=1`；`go run . feishu --help` 及现有 `version/config/mount --help` 仍可用。
- 完成：GUI/CLI 共用 worker；Windows 从不传 --daemon。边界：界面交互归 T10。

### T10：首次使用、冲突弹窗与三方 diff

- 需求：R-01/R-06/R-08/R-09/R-10。依赖：T03/T04/T08/T09。
- 文件：新增 `internal/bridge/ui/{server,session}.go`、`ui/assets/{index.html,app.js,style.css}`、`platform/notify_*.go`、`cmd/feishu/gui.go`、接口/浏览器验收；不修改 `cmd/gui/dist.zip`。
- 输入/输出：GET `/api/status`、POST `/api/auth/start`、POST `/api/auth/complete`、GET `/api/conflicts/{id}`、PUT `/api/conflicts/{id}/draft`、POST `/api/conflicts/{id}/resolve`；ResolveRequest 为统一请求体。
- [ ] 定义 `TestConflictInputsChangedReturns409`、`TestDraftSurvivesCloseAndRestart`、`TestNotifyDeduplicates`、`TestFirstRunWithoutMountWorker`、`TestNoDesktopStillKeepsConflict`、`TestConflictHTMLIsEscaped`、`TestCSRFAndForeignOriginDenied`、`TestPathTraversalDenied`。
- [ ] 运行失败用例。
- [ ] 实现本机认证会话、Origin/Host 校验、首次配置页面、共享目录/挂载点选择和安全状态；任何参数都来自该用户配置。
- [ ] 实现 feishu gui 的每用户 host 和桌面入口；无 worker 时使用 T09 提供的 config/auth 服务，有 worker 时经 IPC 转发，关闭 host 不结束 mount。
- [ ] 实现原版/本地/云端 diff hunks、逐差异选择及结果文本编辑；先保存草稿再请求 resolve；离线只保存草稿；提交复核发生变化保留草稿并返回 409。
- [ ] 实现系统弹窗和首次自动打开，后续同冲突去重；Linux 无桌面不启动浏览器、不丢副本。
- [ ] 验证：`go test -race ./internal/bridge/ui ./internal/bridge/auth ./internal/bridge/store -count=1`；`node --check internal/bridge/ui/assets/app.js`；三平台按 UI 验收步骤实际操作。
- 完成：用户无需安装外部编辑器或手动调用 CLI 即可合并；仅已检测冲突出现提示。

### T11：迁移、恢复、清理与升级契约

- 需求：R-09/R-11/R-13。依赖：T01/T03/T08/T09。
- 文件：新增 `internal/bridge/migrate/{schema,legacy,recovery}.go`、测试夹具；修改现有 Mac/Windows 管理脚本为新入口兼容转发。
- 输入/输出：显式导入旧 settings/rclone remote，生成预览后应用；schema 迁移先备份，未来 schema 旧程序只读拒写。
- [ ] 定义 `TestUpgradeKeepsPendingAndConflicts`、`TestRollbackRefusesNewSchema`、`TestLegacyImportDoesNotExecuteCommands`、`TestIdentityChangeRequiresNewNamespace`、`TestUninstallKeepsUserData`。
- [ ] 运行失败用例。
- [ ] 实现配置预览、路径冲突与身份检查、事务迁移、blob 引用校验、启动项移交；不删除旧缓存或自动运行旧配置中的命令。
- [ ] 实现 GC/恢复页和默认保留数据的卸载；未知写入重启继续核验而非直接重发。
- [ ] 验证：`go test -race ./internal/bridge/migrate ./internal/bridge/store ./internal/bridge/manager -count=1`；保留迁移前/后文件清单和摘要。
- 完成：更新安装目录不能覆盖用户队列；迁移失败可以保留原备份退出。

### T12：三平台打包与发布门槛

- 需求：R-08/R-09/R-13。依赖：T09/T10/T11。
- 文件：新增 `packaging/drivebridge/{README.md,manifest.schema.json,build-macos.sh,build-windows.ps1,build-linux.sh,smoke.sh,smoke.ps1}`、平台安装模板；新增 `.github/workflows/drivebridge-release.yml`；复用现有 `packaging/{macos,windows}/build-package.*` 的运行时/许可证资源。
- 输入/输出：macOS amd64/arm64 pkg/dmg/tar.gz；Windows x64 msi/exe/zip；Linux amd64/arm64 deb/rpm/tar.gz。manifest 记录 Go/CLI/安装工具版本、架构、挂载引擎、SHA-256、签名状态和许可证。
- [ ] 定义打包检查：工具/架构错误、缺少挂载命令、包含开发者路径/账号配置、漏许可证、重复后台启动项时构建失败。
- [ ] 实现按设计 8 的原生构建/安装模板；Go 使用 -trimpath；Windows 包保留 lark-cli/Node.js；CLI 路径从安装位置派生。
- [ ] 实现 CI 单元/竞态测试、原生驱动 smoke、升级保留队列、包安装/卸载、产物校验。缺少真实 runner 或驱动时标未验证并阻止对应平台发布。
- [ ] 本任务 CI 只调用 T01–T11 已存在的包测试及本任务创建的 smoke 脚本；T13 再增加端到端 integration 门槛，不预先调用尚不存在的包。
- [ ] 实现签名/公证为 release job 的必需 secret；缺失可生成明确未签名的测试产物，不能将其作为正式发布成功。
- [ ] 验证：Mac `bash packaging/drivebridge/build-macos.sh --arch arm64 --unsigned`（amd64 在对应原生 runner）；Windows `pwsh -File packaging/drivebridge/build-windows.ps1 -Arch amd64 -Unsigned`；Linux `bash packaging/drivebridge/build-linux.sh --arch amd64 --unsigned`（另跑 arm64）。
- 完成：所有承诺产物都有 manifest 和实际安装/挂载证据。边界：写 workflow 不等于已经发布或通过。

### T13：功能集成、真实挂载与交付审核

- 需求：全部 R-01–R-13。依赖：T01–T12。
- 文件：新增 `internal/bridge/integration/{sync,mount,upgrade}_test.go`、合成测试数据；扩展 `packaging/drivebridge/smoke.*`、现有 packaging/tests 文档及 `.github/workflows/drivebridge-release.yml` 的集成门槛。
- [ ] 实现默认离线的全链路测试：受控 fake cloud 处理状态机/故障；真实接口测试使用 `DRIVEBRIDGE_LIVE=1`、测试 profile 和明确测试目录，不在源码写真实值。
- [ ] 实现本机临时本地后端的真实挂载 smoke，覆盖写入/原子保存/改名/停止；在真实 close/fsync 反馈路径注入数据/元数据 ENOSPC/EIO，不只断言 Go Close；远端模拟和驱动验证分别记录，不能相互替代。
- [ ] 运行 `go test ./lib/larkcli ./backend/feishu ./cmd/feishu ./internal/bridge/... ./vfs/... -count=1`。
- [ ] 运行 `go test -race ./lib/larkcli ./backend/feishu ./internal/bridge/... ./vfs/... -count=1`。
- [ ] 在各发布 OS/架构运行 `go test ./internal/bridge/integration -run TestNativeMount -count=1`，填写挂载驱动、引擎和输出；无桌面和桌面 diff 分别验收。
- [ ] 在隔离合成目录 opt-in 运行 `go test ./internal/bridge/integration -run TestLiveFeishu -count=1`；覆盖小/大/空文件、权限失效、局部补丁、分页、历史不可读及已接受的竞态反例。
- [ ] 按 implementation-decisions 的 LV-01–LV-05，在明确授权的隔离账号/目录执行实际凭据共享、逐操作能力、精确元数据、完整分页与嵌套 DTO 契约验收；未通过的能力保持关闭，不借 mock 放行。
- [ ] 记录两台电脑独立授权/同账号与不同账号隔离；完成 T11 升级/恢复验收；检查 UI 无“所有冲突都能避免”的宣传。
- [ ] 进行需求覆盖与整分支审核；修复阻塞项并重验受影响检查。尚缺的平台/签名/真实场景写成未完成，不宣称全部交付。
- 完成：每项需求都有产品行为和证据，未上传数据保留且恢复路径可操作；用户审阅后再决定合并/发布。

## 6. 依赖与完成条件

执行顺序：T01→T02/T03→T04→T05→T06→T07；T08 在 T03 后独立完成；随后 T09→T10/T11→T12→T13。用户已授权多 agent；研究/测试设计/独立复核可并行。共享文件和规范接口按所有者交接，不同时写 model/types.go、cloud/client.go、cmd/feishu 或发布 workflow。T02 先定义 coordinator 注入契约，T08 后实现，T09 组装后才启用需凭据的生产调用，避免 T02↔T08 循环依赖。

每任务提交范围只包含自己的文件和必要契约修订；提交前运行相应验证。共享接口需要调整时，先修改设计与本文、记录原因及受影响任务，不允许相邻任务各自发明兼容层。

CI/本地缺驱动、签名、权限或真实环境时记录具体未执行项和恢复条件；不使用被跳过的测试数、成功 mock、CLI 帮助输出或交叉编译证明实际挂载通过。

## 7. 写作前关卡与复核

v1.1 require-ready/95 分为历史；v1.2 的真实能力及独立复核仍未完成，当前 [development-gate.json](development-gate.json) 不宣称 ready。保守能力失败协议允许离线模块继续开发，不构成对应云端能力发布许可。

本地接口实验的结论只能引用 [validation-results.md](validation-results.md)；本文所有新增产品测试尚未运行。历史实施者复核见 [review.md](review.md)；v1.2 仅完成作者自查，review_status=pending，等待独立复核。元数据 SHA 绑定当前 pending 设计，只说明内容对应，不代表复核通过。Task 0 的 T01–T13 自洽及共享边枚举记录在执行账本 `.superpowers/sdd/development/preflight.md`。
