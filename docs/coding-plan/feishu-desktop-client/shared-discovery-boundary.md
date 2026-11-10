# “全部共享目录”能力核验

2026-11-10；用户明确要求：通过一次授权链接及输入框登录后，自动列出全部共享目录，不能默认为逐条粘贴共享根链接。此项当前是未解决的接口条件；未将范围改写为“已添加目录”。

## 已核实事实

1. 官方 CLI 1.0.99 的实际源码版本为 8d8357d39dd15ac5e9aa815d4ab2639d56bd3239； inspected 1bfd 的相关 Drive/Wiki/auth 文件与其一致。
2. Drive files.list 的 schema 明确：空 folder_token 列用户云空间根，根第一层不支持分页、不返回快捷方式。普通目录可以按自身 token 和 next_page_token 完成枚举；根返回列表不能证明覆盖所有共享根。
3. 官方 root_folder/meta 返回用户自己的 root token；没有证明该 token 能让“与我共享”全部根目录出现。
4. 已检查的 CLI Drive catalog/shortcuts 未找到有完整性保证的 shared-with-me 根目录枚举器。这个结论只说明当前集成能力缺口，不声称飞书所有其他接口永远不存在。
5. drive +search / Search v2 可以空关键词、按 folder/owner/sharer 等过滤，但其搜索结果没有被核实为完整目录库存。不能凭搜索翻页结束声明无遗漏。
6. Wiki 的个人文档库 my_library 与 Drive 根不是同一个资源；Wiki space-list 本身也不覆盖个人文档库。
7. OAuth scope、用户对文件的实际权限、目录发现能力是三个不同条件。增加 scope 不能使用户取得未共享内容的访问权，也不会增加目录接口本身的枚举语义。
8. 对本机官方 1.0.99 仅运行了隔离配置的 `drive files list --help`、`api --help`，没有云端请求。该版本支持 typed --folder-token/--page-size/--page-token，也支持 --params。当前主线技能文档关于无 typed flags 的描述不能直接覆盖这个已锁定版本。

## 可继续的独立开发

托盘真实登录名/授权状态、冲突数字、多人文件 diff 草稿及逐文件批量提交均不依赖全共享目录枚举，可继续。账户级自动虚拟盘的“全部目录”验收保留为未满足，不拿这些界面能力替代它。

若必须完整自动发现，需要找到并验证具有完整性语义的官方列举能力，或者明确一个覆盖所有共享内容的已授权共同根目录。仅扩大登录授权、输入一个 OAuth 链接或使用搜索补充，都不能直接作为“全部共享目录”的证明。

## 一手依据

- https://github.com/larksuite/cli/blob/8d8357d39dd15ac5e9aa815d4ab2639d56bd3239/internal/registry/catalog/services/drive.json
- https://github.com/larksuite/cli/blob/8d8357d39dd15ac5e9aa815d4ab2639d56bd3239/shortcuts/drive/drive_search.go
- https://github.com/larksuite/cli/blob/8d8357d39dd15ac5e9aa815d4ab2639d56bd3239/shortcuts/drive/drive_move.go
- https://open.feishu.cn/document/server-docs/docs/docs-overview?lang=zh-CN

以上为调研事实与缺口记录，不是已完成实现或正式交付声明。参考源码仍位于忽略的 third_party，不纳入版本控制。
