# Markdown 转换规则

DriveBridge 将兼容转换声明在 JSON 中。安装包内置默认规则，无配置文件时行为保持一致：普通段落中的简单 `<br>`（包括大小写、空格和自闭合变体）转换成换行；表格单元格中的简单 `<br>` 转换成 `; `；独立的简单 `<br>` 行只保留段落间距。

## 管理界面入口

点击系统托盘或菜单栏的 DriveBridge → 打开管理页 → **账号与目录 → 后台自动替换**。该卡片常显在设置页顶部，作用于本机当前用户的所有账号。

可以直接编辑原始字符串、转换目标、作用位置及匹配方式，点击“新增规则”追加记录，再点击“保存规则”。目标类型区分文本、换行和删除；选择“删除”才表示移除匹配内容，右侧删除按钮则用于移除自定义规则。内置换行规则的匹配边界固定，目标可修改。内置 YAML 元数据与普通表格结构规则的所有字段只读，不能改成删除或文字替换。

如果保存后提示重启，重新启动 DriveBridge 后应用新规则；正在同步的进程继续使用原有规则，不在操作中途更换转换语义。设置页显示实际 JSON 文件位置及只读预览，也保留直接编辑文件的方式。状态轮询不会覆盖未保存的规则；重新读取前会提示放弃编辑，检测到其他页面或文件修改时不会覆盖旧版本。

## 用户文件

桌面设置初始化配置后，会在实际使用的 `config.json` 同一目录生成可编辑的 `conversion-rules.json`。默认路径：

- macOS：`~/Library/Application Support/DriveBridge/conversion-rules.json`
- Windows：`%APPDATA%\DriveBridge\conversion-rules.json`
- Linux：`$XDG_CONFIG_HOME/DriveBridge/conversion-rules.json`，未设置时为 `~/.config/DriveBridge/conversion-rules.json`

若使用自定义配置路径，规则文件也放在该配置文件旁边。初始化只创建缺失的规则文件，使用私有权限和完整文件的原子发布；已有用户文件不会被覆盖。修改后重启 DriveBridge 生效。缺少文件时运行时仍有内置默认规则；存在但格式错误、无法读取或校验失败时，启动会报告文件路径和规则错误，不会悄悄忽略错误。

初始化模板包含 YAML 元数据、普通表格结构和已观察到的简单 `<br>` 兼容规则。HTML 换行用一个逻辑匹配覆盖 `<br>`、`<br/>`、`<br />` 等变体；不会预设删除不换行空格、零宽字符等内容。

初始文件内容如下，两个换行规则覆盖上述三种写法，也兼容大小写：

```json
{
  "version": 1,
  "rules": [
    {"id":"simple-break-paragraph","source":"<br>","target":"\n","context":"paragraph","match":"simple_html_break"},
    {"id":"simple-break-table","source":"<br>","target":"; ","context":"table","match":"simple_html_break"},
    {"id":"yaml-front-matter","source":"---","target":"yaml_code_block","context":"document","match":"front_matter"},
    {"id":"plain-table-resize","source":"table","target":"preserve_cells","context":"document","match":"table_resize"}
  ]
}
```

文档第一个字节开始、以独立 `---` 行包围的有效 YAML 映射，会作为原生 YAML 代码块上传。完整保留两个分隔符、所有字段、注释、空格和换行，不执行 YAML 标签，也不套用普通文字或 HTML 替换。普通水平线、代码围栏内的 YAML 和 `+++` TOML 不触发此规则；缺少结束分隔符或语法损坏的 YAML 元数据会阻止写回并报告具体原因。代码块第一行使用固定注释 `# DriveBridge YAML front matter v1` 标记，后面保留原始元数据；该标记放在代码正文中以保证官方 API 读回时仍存在。只有文档首个 YAML 代码块上的该完整标记会在新投影版本 3 中移除标记并还原元数据。已有版本 1/2 检查点继续按原版本加载；后台新读回含该标记的文档时才升级到版本 3。

`plain-table-resize` 允许既有安全普通表格路径在后台处理增删行列，保留单元格内容及可安全保留的布局。它不扩展富表格、合并单元格或资源内容的写回权限。结构规则是固定的内置策略；旧用户 JSON 未包含它们时，会自动补全，不必手工修改文件。

段落换行只有在编码前后语义一致时才转换为 Markdown 硬换行。连续或尾部换行、列表及引用等不能无损转换的情形，保留原始表示，由后台 XML 编码器转换并校验云端读回；不会为了移除标签改变排版结构，也不改变已有缓存的投影版本。

例如，追加普通文字字符串转换，并增加 `<wbr>` 在 Markdown 表格中的转换：

```json
{
  "version": 1,
  "rules": [
    {
      "id": "rename-text",
      "source": "旧名称",
      "target": "新名称",
      "context": "paragraph",
      "match": "literal"
    },
    {
      "id": "table-separator",
      "source": "<wbr>",
      "target": "; ",
      "context": "table",
      "match": "exact"
    }
  ]
}
```

`rules` 数组可以继续追加记录，没有固定条数上限。为限制内存和解析开销，文件最多 8 MiB，每条的 `source` 和 `target` 各最多 64 KiB。每条必须有全部五个字段，ID 必须非空且唯一，字段必须显式指定，不能为 `null`；同一上下文不能有两个相同来源。未知字段、未知版本、重复匹配和非法转换目标都会报错。

| 字段 | 含义 |
| --- | --- |
| `id` | 规则的稳定名称，例如 `word-separator`。 |
| `source` | `literal` 为非空的普通单行字符串；`exact` 为完整 HTML AST token，例如 `<wbr>`，不能是多个标签或跨行片段。 |
| `target` | `literal` 为普通单行字符串；HTML 转换规则为安全的行内 Markdown，支持文字、强调、加粗和代码片段，`"\n"` 表示换行。显式空字符串 `""` 表示有意删除匹配内容。前后空格会保留。 |
| `context` | `paragraph` 用于表格之外的行内内容（含标题、列表等）；`table` 用于 Markdown 表格单元格；`document` 仅供固定内置结构规则使用。 |
| `match` | `literal` 在普通文字节点内匹配字符串；`exact` 按完整 HTML token 区分大小写匹配；`simple_html_break` 为内置简单 `<br>` 匹配，不接受属性；`front_matter`、`table_resize` 仅供固定内置结构规则使用。 |

`literal` 的来源和目标是普通字符串，不跨行，不跨 Markdown 标记或文字节点；规则不能用来插入格式、HTML、链接或图片。替换目标必须稳定，不能再次触发同一上下文的其他规则；直接产生连锁替换的配置会在加载时被拒绝。后台还会检查规范化结果再次应用规则是否保持一致；若上下文拼接产生新的匹配，任务会报告规则规范化错误并停止写回。

HTML 转换目标中的 XML 特殊字符会正确转义。目标不允许 HTML、链接、图片、控制字符、块级结构和多行内容（单个 `"\n"` 除外）。例如 `"**注意** & 更多"` 会产生加粗文字和可见的 `&`；`"<script>..."` 或 `"[x](javascript:...)"` 会被拒绝。

用户文件中的新规则自动补充到默认规则之后。若要修改默认目标，可使用默认 ID `simple-break-paragraph` 或 `simple-break-table`，保留其 `source: "<br>"`、原有 `context` 和 `match: "simple_html_break"`，只修改 `target`。也可以用 `exact` 精确匹配某种写法；精确匹配优先于简单 `<br>` 通配匹配。

后台进入 Git 三方合并前，基线、本地和远端三份输入副本都会使用同一份规则规范化，避免 `<br>` 写法等兼容差异形成伪冲突。规范化只处理后台副本，不在前台保存路径增加工作，也不原地修改已保存的检查点或远端解码投影。

本地保存仍先完成持久化；替换、合并和上传在既有后台同步周期执行，不要求点击按钮或调用 CLI。旧冲突的草稿如果与正文仅有这些编码等价的差异，也可自动恢复同步；如果存在实质正文差异，继续保留草稿与原始 Git 历史，不自动丢弃。

转换通过 Markdown AST 限定范围：`literal` 只处理普通文字节点，HTML 规则只处理对应的 HTML 节点。行内代码、围栏代码、缩进代码、链接及其文字、URL、图片及其说明、转义文字不会被替换。规则也不会穿透包含图片等资源的受保护原始 HTML 表格。未知 HTML 和不支持的富内容仍会阻止写回。

## 程序入口

```go
path := markdown.ConversionRulesPath(paths.ConfigFile)
rules, err := markdown.LoadConversionRules(path)
if err != nil {
    return err
}
if err := markdown.ConfigureConversionRules(rules); err != nil {
    return err
}
```

管理服务在 Markdown 解码、规划和写回开始前配置规则。`ConversionRules` 是不暴露内部表的不可变快照。首次配置或首次运行时使用会冻结当前进程的快照；重复配置同一内容允许成功，替换不同内容返回 `ErrConversionRulesRestart`，要求重启。编码、规划以及用于验证操作的语义哈希使用同一转换语义，解码后的远端内容保持原样。

`DefaultConversionRulesJSON()` 返回内置 JSON 的独立副本。`EnsureConversionRulesFile(configFile)` 在配置目录初始化后，以同目录私有临时文件、文件同步、无覆盖硬链接发布和目录同步生成模板；路径中的符号链接及非普通目标文件会被拒绝。已有文件保持原样，运行时加载和校验仍由 `LoadConversionRules` 完成。
