# 实施阶段接口证据（2026-11-10）

本目录记录主协调者对已授权隔离实验目录的调用；与此前只读源码审核分开。先使用现存合成文件读取 raw history、指定 V1/V2/V1 和逐页列表；随后新建一个合成在线文档，验证 XML 替换/插入/删除及所列 Markdown 子集。保留测试文档用于追溯，未清空实验目录。测试状态文件中的真实文档引用留在被忽略执行工作区；此目录不包含账号凭据、真实文档 ID 或个人正文。

- cli-binary.json：实际调用二进制版本及 SHA-256；不能由同版本号推导源码与二进制可复现对应。
- raw-history-readonly-probe.json：only_tag=false 样本、字段存在与类型；不证明 current head。
- version-download-readonly-probe.json：V1/V2/V1 为45/81/45字节及不同/重复稳定哈希；不证明重命名/revert/删除后的通用版本语义。
- single-page-readonly-probe.json：page_size1 的四页完整样本；错误注入和根目录语义另验。
- docx-mutation-probe-result.json：新合成文档段落替换、插入、删除及非目标原 XML 保留。
- docx-subset-probe-result.json：所列标题（含跳级h6）、行内样式/字面尖括号/链接查询、多行代码与尾换行、有序/无序列表、引用、简单表格的具体检查。ordered seq 的auto被规范为1，显式3保留；不是任意列表/布局无损证明。
- docx-subset-probe-fixtures.json：对应 full XML 已把所有远端 ID 改为合成稳定标签，供 codec 回归；这不是实际远端资源引用。
- file-observation-resolution.md：独立只读来源分析及观测路线的实施推断，不能当作它执行了上述真实试验。

结果支持这些精确 payload/样本；未执行响应丢失注入、多人/两机授权、所有 Markdown 变体、服务器 CAS 或幂等重放。能力启用必须继续绑定 CLI 版本/二进制、codec 子集和 T07 恢复验收，不能仅把命令退出0当作产品通过。
