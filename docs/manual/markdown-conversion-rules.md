# Markdown 转换规则

DriveBridge 将兼容转换声明在 JSON 中。安装包内置默认规则，无配置文件时行为保持一致：普通段落中的简单 `<br>`（包括大小写、空格和自闭合变体）转换成换行；表格单元格中的简单 `<br>` 转换成 `; `；独立的简单 `<br>` 行只保留段落间距。

## 用户文件

在应用 `config.json` 的同一目录创建 `conversion-rules.json`：

- macOS：`~/Library/Application Support/DriveBridge/conversion-rules.json`
- Windows：`%APPDATA%\DriveBridge\conversion-rules.json`
- Linux：`$XDG_CONFIG_HOME/DriveBridge/conversion-rules.json`，未设置时为 `~/.config/DriveBridge/conversion-rules.json`

修改后重启 DriveBridge 生效。应用不会自动创建或改写这个文件。文件不存在时使用内置规则；存在但格式错误、无法读取或校验失败时，启动会报告文件路径和规则错误，不会悄悄忽略错误。

例如，增加 `<wbr>` 在普通段落和 Markdown 表格中的转换：

```json
{
  "version": 1,
  "rules": [
    {
      "id": "word-separator",
      "source": "<wbr>",
      "target": " / ",
      "context": "paragraph",
      "match": "exact"
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

`rules` 数组可以继续追加记录，没有固定条数上限。为限制内存和解析开销，文件最多 8 MiB，每条的 `source` 和 `target` 各最多 64 KiB。每条必须有全部五个字段，ID 必须非空且唯一，同一上下文不能有两个相同的精确来源。未知字段、未知版本、重复匹配和非法转换目标都会报错。

| 字段 | 含义 |
| --- | --- |
| `id` | 规则的稳定名称，例如 `word-separator`。 |
| `source` | 一个完整的 HTML AST token，例如 `<wbr>`，不能是任意文本、多个标签或跨行片段。 |
| `target` | 安全的行内 Markdown，支持普通文字、强调、加粗和代码片段。`"\n"` 表示换行；单独出现的换行标签作为段落间距处理。前后空格会保留。 |
| `context` | `paragraph` 用于表格之外的行内内容（含标题、列表等）；`table` 用于 Markdown 表格单元格。 |
| `match` | 自定义规则使用 `exact`，按完整 token 区分大小写匹配。内置规则使用 `simple_html_break`，只识别简单 `<br>`，不接受属性。 |

目标中的 XML 特殊字符会正确转义。目标不允许 HTML、链接、图片、控制字符、块级结构和多行内容（单个 `"\n"` 除外）。例如 `"**注意** & 更多"` 会产生加粗文字和可见的 `&`；`"<script>..."` 或 `"[x](javascript:...)"` 会被拒绝。

用户文件中的新规则自动补充到默认规则之后。若要修改默认目标，可使用默认 ID `simple-break-paragraph` 或 `simple-break-table`，保留其 `source: "<br>"`、原有 `context` 和 `match: "simple_html_break"`，只修改 `target`。也可以用 `exact` 精确匹配某种写法；精确匹配优先于简单 `<br>` 通配匹配。

转换只在 Markdown AST 中对应的 HTML 节点生效。行内代码、围栏代码、转义文字和 URL 目标不会被替换。规则不会改写本地检查点字节或远端解码投影，也不会穿透包含图片等资源的受保护原始 HTML 表格。未知 HTML 和不支持的富内容仍会阻止写回。

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

`DefaultConversionRulesJSON()` 返回内置 JSON 的独立副本，可用于创建编辑模板；库本身不写用户文件。
