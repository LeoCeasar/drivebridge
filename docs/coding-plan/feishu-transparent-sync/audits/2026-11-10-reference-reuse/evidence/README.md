# 离线兼容性复现

`docx-compatibility-test.go.txt` 是测试源码快照，使用假 Runner 返回合成 JSON，从不执行 lark-cli。`.txt` 后缀避免它进入主仓库正常 Go 包扫描。

复现需要本机 Go 1.25+ 和本报告固定提交的 `third_party/larkfs`。将该测试复制为临时目录中的 `docx_test.go`，建立以下 `go.mod`，把占位路径替换为当前本地克隆的绝对路径：

```go
module larkfs-contract-probe

go 1.25.0

require github.com/IchenDEV/larkfs v0.0.0

replace github.com/IchenDEV/larkfs => /ABSOLUTE/PATH/TO/drivebridge/third_party/larkfs
```

仅在这个临时目录运行：

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -v -count=1 .
```

当前固定源码下预期退出 1，两条断言失败；这是参考适配器的缺陷复现，不是主产品测试失败。实际输出见 `docx-compatibility-result.json`。若上游已更新，先确认提交，不应据此覆盖原证据。

JSON 输入形状依据：官方 `shortcuts/doc/docs_fetch_v2.go:87–93`、`docs_create_v2.go:130–137`，以及主工作项此前已保存的 `21-roundtrip-initial-md.json`、`19-create-roundtrip-doc.json`。此次没有重跑真实飞书操作，也没有使用其中真实资源 ID。
