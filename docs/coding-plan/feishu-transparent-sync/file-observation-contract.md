# 普通文件观测快照实施契约

2026-11-10。这是 R-01/R-05/R-10/R-11 的实施裁决：保留用户已确认的尽力同步及并发窗口，不新增服务器版本、CAS 或完整历史保证。适用 T02/T03/T05/T06/T07/T11/T13，优先于 v1.2 中“没有当前版本关联便阻止所有普通文件访问”的措辞。

## 两条独立路线

- 服务器版本路线：FileMeta/指定版本下载只接受真实服务器版本。历史记录首项、最大编号或请求回显不能证明 current；尚未验证的能力返回 metadata_unknown/unsupported_capability。
- 下载观测路线：成功的、经认证的整个文件下载，构成一次传输已完成的字节观测。验证输出文件及成功信封后计数和 SHA-256，持久采用这些字节。它没有服务器版本，不调用 --version，也不声称下载结束时的全局 head。

观测保证本机快照的大小与读取字节严格一致。HTTP 已声明长度的提前 EOF 会经 Go transport/body/io.Copy 产生错误；失败的下载不采用。计数及 SHA 不能独立证明服务端从未返回语义错误的内容，也不能证明并发修改中的远端原子快照。未暴露的 HTTP 状态/响应头不能伪称已检查；这些限制保留为真实完整性验收。

## 规范模型（T02 必要的 T01 模型扩展）

`internal/bridge/model` 是唯一所有者；lib/larkcli 的 wire 类型独立，cloud 负责转换。

```go
type FileIdentityKind string
const (
    FileIdentityServerVersion FileIdentityKind = "server_version"
    FileIdentityObservation FileIdentityKind = "observed_download"
)
type FileIdentity struct {
    Kind FileIdentityKind `json:"kind"`
    Version string `json:"version,omitempty"`
    ObservationID string `json:"observation_id,omitempty"`
    SHA256 string `json:"sha256"`
    StartedAt time.Time `json:"started_at"`
    CompletedAt time.Time `json:"completed_at"`
}
```

观测 variant 必须有 UUID ObservationID、64 位小写 SHA-256、有效起止时间，Version 必须为空。服务器版本 variant 必须有真实非空 Version、ObservationID 必须为空；形成持久字节快照时同样必须有实际 SHA。两个 variant 不可混合。只有显式检查 Kind 后才能选择服务器版本 API。

`model.Snapshot` 新增 `FileData BlobID`（file_data,omitempty）及 `FileIdentity *FileIdentity`（file_identity,omitempty）；二者用于普通文件。原 Revision 继续用于在线文档，不塞入观测哈希；XML/Markdown/BlockMap 不保存普通二进制。普通文件持久快照要求 FileData、身份、SHA 和实际字节数相符。GC 必须包含 FileData 引用。

## T02：获取与临时所有权

增加 cloud.Client.ObserveFile(ctx, token) 返回 (*FileObservation, error)，其中 FileObservation 拥有临时文件租约；它包含 Token string、Identity model.FileIdentity、Size int64 和 Open() (io.ReadCloser,error)/Close() error，不向上层提供任意路径。lib/larkcli 提供独立 wire/transport 对象，由 cloud 转换；不得导入 internal/bridge。Close 幂等，调用方采用或放弃后必须释放临时资源。

完整下载在私有临时目录运行，不请求远端 Range。只接受进程成功、明确成功信封、匹配身份、存在且非负的整数 size_bytes；saved_path 必须是该目录内的普通文件且没有链接逃逸。实际读取/统计/报告大小及 SHA 一致后才返回租约。取消、非零退出、partial、字段缺失、大小不符、路径逃逸均不返回可采用对象。大文件流式处理。

严格 FileMeta 仍可返回 metadata_unknown；这不能禁止 ObserveFile 路线。禁止把观察哈希放入 FileMeta.Version 或服务器 revision/version 参数。没有经过采用的临时文件不代表本机持久保存。

## T03：持久采用、引用与恢复

先在事务/发布锁外获取、导入并验证 blob；同步文件、原子改名、同步目录后，短事务绑定普通文件 Snapshot。保存 snapshot 与初次采用分开：初次采用只允许干净、无待上传、未冻结且预期代次/Token 匹配的 entry；不得覆盖本地新编辑。失败不发布，孤立 blob 按既有宽限 GC。

Store 增加三项明确职责，具体导出签名在 T03 brief 冻结：保存文件 snapshot（含字节/身份验证）、初次干净采用、将远端备份 snapshot 作为作业 recovery 引用保护。刷新仍走 OfferRemote，上传回读仍走 Confirm；不能复用初次采用绕过发布屏障。ReadBlob 的在用租约与所有 snapshot/recovery/pending 引用参与 GC。

## T05/T06：准确属性和同一快照读取

ManagedMaterializer 同时处理未物化在线投影及普通文件。普通文件首次目标 Stat/Getattr/Lookup/Open 可以按需下载并持久采用；目录枚举不获取全部正文。采用后对象大小和全部 Open/Range/Seek 必须绑定同一 FileData；不能 Stat 测一次、Open 又下载另一个版本。失败传播错误，不以零字节代替。VFS 空闲发布、句柄和命名空间规则不变。

原生 direct NewObject 可以同步获取并绑定目标对象的完整观测；元数据 List 不为凑大小下载全部文件，也不能把未知大小标成零。原生挂载是否能处理未知属性须有对应接入证据；不能把托管 Materializer 的保证外推过去。

## T07：备份、覆盖和确认

每次覆盖/删除前持久保护新获取的远端观测和本地输入；获取或持久保护失败即暂停该 mutation。备份仅包含实际已观察到的远端字节，不能保证包含随后另一个人的编辑。

同 Token 覆盖后再次完整观察并比较捕获作业输入的 SHA/大小；匹配仅表示上传后观察到目标内容，不证明请求因果或永久不变。超时先回读，不能盲重发。不同内容保持未确认/冲突；相同内容的 ABA 和两次轮询间的修改不可检测。普通文件用观测内容哈希比较，Docx/已验证版本路线才使用服务器 revision。

## 验收

本机测试：variant 类型检查；实际空文件与缺字段；进程失败/取消后残留文件不可采用；路径/链接/计数/哈希错误；blob 同步/事务失败；代次或冻结变化；重启后读原 blob；打开句柄期间刷新延期；GC 保留在用与 recovery；备份落盘早于覆盖；响应丢失先回读。mock 只证明本机分支。

真实验收：已知空/非空/大文件的完整下载与独立预期哈希，覆盖/改名后读取，截断/取消与并发观测边界。既有 81 字节回读和 V1/V2/V1 样本只支持各自范围。服务器 current-version、历史保留/revert/删除版本等强语义仍须分别验证，不能成为关闭普通观测读取的通用前置条件。
