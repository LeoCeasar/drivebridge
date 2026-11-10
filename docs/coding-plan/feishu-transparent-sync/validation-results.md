# 飞书透明同步：真实接口验证结果

日期：2026-11-10。首次实验结论：基本读写已验证，原方案的严格并发保护假设不成立。后续用户已明确接受尽力合并/检测/恢复及残余覆盖风险，据此形成 design.md / development.md；这不改变下面的实验事实，也不表示产品测试已通过。

本文只记录事实、影响和待决定项，不是正式功能方案或产品实现文档。

## 环境与实验边界

- 本地项目基线：`eafd1257d255e00f38aed058eaf24afd2d3cb4f5`。
- macOS arm64；官方 `@larksuite/cli` 1.0.99。CLI 二进制 SHA-256 见 [manifest](evidence/2026-11-10/manifest.json)。
- 用户提供独立文件夹并完成应用配置及用户身份授权。实验使用独立配置目录；产品不得依赖本机路径、账号或该测试目录。
- 在用户指定目录内创建一个测试子目录、3 篇在线文档、1 个普通文件。只修改本轮创建的资源；没有修改、删除已有业务文档，没有变更分享权限或所有权。
- 测试资源保留供复查：[测试目录](https://pcnm0xjfs021.feishu.cn/drive/folder/NpsFflI2RlYxjrdfkeWcHI7Xn7g)。
- 证据为脱敏的请求参数、响应、退出码和回读内容。账号密钥、登录 device code、访问/刷新令牌和授权响应不进入仓库。

## 结果

| ID | 实验与实际结果 | 结论 |
|---|---|---|
| V-01a | `docs +update`：基线版本 3；先改为“120 万”产生版本 4；再携带旧版本 3 对同块写“130 万”，返回 success，回读版本 5 仅剩“130 万”，没有冲突警告 | `--revision-id` 不能作为本路径的 compare-and-swap 条件 |
| V-01b | 公开 `docx/v1/.../blocks/:block_id` PATCH：基线版本 5；先写“140 万”产生版本 6；再次携带版本 5 写“150 万”，仍返回成功并产生版本 7 | 对本次测试的 `update_text_elements` 请求，公开块接口也未原子拒绝过期写入 |
| V-02a | 使用同一旧版本分别改两个不同段落，两处修改均保留，最终版本 9 | 局部更新能缩小覆盖范围；不能据此推断同段修改受到保护 |
| V-02b | 在最新版本已推进后，以版本 4 回读，成功取得当时的“120 万” | 本轮近期历史可读，有助于恢复；尚未证明长期保留、断网后恢复或所有文档均可读 |
| V-03a | 创建包含标题、中文、列表、代码、表格、链接和转义字符的 Markdown 文档；追加高亮块；只替换一个段落 | 回读确认改动生效，原代码、表格、高亮块的完整 XML（含块 ID）保持相同 |
| V-03b | 将官方 Markdown 导出结果原样导入另一篇测试文档 | 字面量 `<tag>` 被删除；响应带 `degrade_code=4010`。成功响应不能等同于内容无损 |
| V-03c | `docs +fetch --doc-format markdown --detail full` | 返回 warning，说明 Markdown 不支持 full detail；需要单独获取结构和块 ID，不能只靠导出的 md 建立精确映射 |
| V-04 | 普通文件从 45 字节覆盖为 81 字节，使用原 file_token 下载回读 | file_token 不变；返回版本变化；回读大小为 81，SHA-256 与上传内容一致 |
| V-05 | 测试目录设 page_size=1，page-all=true，page-limit=0 | 4 个资源被聚合到单个 JSON 信封的 data.files，has_more=false。当前 CLI 的该输出可被现有 listData 解析；不应笼统宣称分页未实现 |
| V-06 | 对同一普通文件调用 +version-history，limit=2 与 limit=1 | 最新版本 size_bytes=81，与下载一致；旧版本为45。提供了小文件大小元数据依据，不证明所有身份/文件都支持该能力 |

以上均为当前账号、目录、CLI 版本及具体请求的结果，不是跨租户、跨版本或所有操作类型的保证。

## 关键复现依据

### 旧版本并不会自动拒绝写入

实验由两个连续操作模拟不同写入者，关键顺序是“读取基线 → 另一操作更新远端 → 基于旧基线提交”。不需要真的同时发包即可验证过期版本语义；这不是多人 UI 压力测试，也尚未验证网页编辑器的操作转换协议。

`docs +update` 的精确 argv 与响应见：

- [原始基线版本 3](evidence/2026-11-10/03-conflict-base.json)。
- [较新写入成功，产生版本 4](evidence/2026-11-10/04-fresh-edit.json)。
- [旧版本 3 的同块写入仍成功](evidence/2026-11-10/06-stale-same-block.json)。
- [回读确认变为 130 万](evidence/2026-11-10/07-after-stale.json)。

公开块接口使用：

```text
PATCH /open-apis/docx/v1/documents/{document_id}/blocks/{block_id}
query: document_revision_id=5, client_token=<独立操作 UUID>
body: {"update_text_elements":{"elements":[{"text_run":{"content":"预算为 150 万。"}}]}}
```

在 head 已为 6 时请求仍成功，见 [过期 PATCH 的响应](evidence/2026-11-10/12-native-stale-edit.json) 和 [回读](evidence/2026-11-10/13-after-native-stale.json)。`client_token` 用于幂等，不是针对其他写入者的排他锁。

补充负向对照：不存在的未来版本 999999 被拒绝，CLI 退出 1、错误码 4000002，见 [未来版本请求](evidence/2026-11-10/08-future-version.json)。这与“过期但存在的版本可写”不是同一种行为。

公开 API 参数来源：[更新块的内容](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document-block/patch)。本轮同时读取了该页面 `.md` 版本；其版本字段描述并未承诺过期即拒绝。

### Markdown 原样往返会改变内容

局部修改前后，非目标块保持一致，见 [修改前](evidence/2026-11-10/27-with-complex-block.json) 与 [修改后](evidence/2026-11-10/29-after-targeted-xml.json)。

将导出的 Markdown 重新导入后，同一字面段落发生：

```text
之前：*星号*、x_y、a[b]、<tag>、C:\Users\test。
之后：*星号*、x_y、a[b]、、C:\Users\test。
```

证据：[Markdown 导出](evidence/2026-11-10/30-after-targeted-md.json)、[带降级 warning 的创建响应](evidence/2026-11-10/31-create-reimport-doc.json)、[重导入后的结构](evidence/2026-11-10/32-reimport-xml.json)。代码块内相同的 `<tag>` 仍保留，问题位于正文的格式解释，不能用全局字符串搜索代替定位检查。

### 普通文件覆盖保持身份及内容

证据：[首次上传](evidence/2026-11-10/22-upload-file.json)、[原 Token 覆盖](evidence/2026-11-10/23-overwrite-file.json)、[下载回读](evidence/2026-11-10/24-download-overwritten.json)、[大小/Token/SHA-256 检查](evidence/2026-11-10/25-overwrite-check.json)。

本轮未测试超时后提交结果不明确、大文件分片、零字节文件、版本历史权限继承及长期恢复。不能将小文件结果直接扩展为这些能力已通过。

## 对设计的影响

1. 透明 Markdown 编辑需要基线、结构侧车、局部补丁及明确的内容保真范围。原样导出/重导入不满足无损要求。
2. 写入前重读、三方合并、写后校验和本机锁都可以减少风险，但无法单独消除飞书网页或其他电脑参与的检查后写入竞态。
3. 冲突弹窗和 diff 只能处理已检测到的冲突。接口可能先成功覆盖，不能承诺所有冲突都在覆盖前弹窗。
4. 本地备份和可读历史版本可以辅助恢复；本轮没有证明任意断网时长后仍可完整恢复，不能以此替代原子并发保护。
5. “尽力同步并提供恢复”和“严格拒绝任何过期覆盖”是不同交付承诺。用户已在阅读本轮结果后明确选择前者，见 recon.md R-10；此变更已公开记录。

## 后续设计与验证

- 用户已接受网页与本地混合编辑采用尽力检测及恢复机制，明确存在未被原子拒绝的竞态窗口。
- Markdown 子集/复杂块保护、编辑器替换保存和 VFS 一致性已形成正式契约并经文档复核，实际测试仍须按开发任务执行。
- 超时结果不明确的核验、大文件、断网/磁盘满/崩溃、跨用户配置、三平台挂载及弹窗/diff 的实现验证。

本轮只完成接口调研实验，没有实现产品功能，也没有执行完整跨平台验收。观测断言见 [verified-observations.json](evidence/2026-11-10/verified-observations.json)，断言全部成立表示上面记录的现象得到证据支持，不能解读为原设计全部通过。
