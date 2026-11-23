# macOS / Windows 0.2.32 预发布构建记录

用户于 2026-11-23 明确授权发布目录发现修复。范围为 macOS Apple Silicon 与 Windows x64 的完整飞书桌面包；保留功能分支，不合并 main。

## 源码及发布

- 安装包共同源码与远端标签目标：`0009ff3dc3286e9c206bbdee2d3812b4adb1bc14`。
- 两个平台 CLI / 桌面 Go 可执行文件均核对该提交，`vcs.modified=false`。
- [0.2.32 发布页](https://github.com/LeoCeasar/drivebridge/releases/tag/drivebridge-v0.2.32)。

## 验证

- 完整本地 Go 套件、相关竞态、43 项 UI、54 项打包测试通过，详见[目录发现修复记录](2026-11-23-directory-freshness.md)。
- [Windows 原生验证 45165072760](https://github.com/LeoCeasar/drivebridge/actions/runs/45165072760)：21 个共享包、2733 个测试通过事件；四项 WinFsp 门禁、七项剪贴板/提示原生回归、43 项 UI 通过，必需原生门禁无跳过。
- [Windows 构建 45168836314](https://github.com/LeoCeasar/drivebridge/actions/runs/45168836314)成功。工作流核对并复用同一源码的原生验证，构建中跳过的重复测试不视为新增测试证据。
- Windows ZIP 的 64 项载荷逐一核对，内嵌清单与产物清单一致；PE 子系统 CLI=3、桌面=2。
- macOS 四项原生门禁通过；便携载荷 23 项、DMG 完整性、图形安装器内 27 项应用文件及内嵌清单均核对通过。
- macOS 包与现场已安装修复版一致：此前从 0.2.29 安全升级到 0.2.32，目标父目录从 11 项恢复为 12 项，配置保持一致。
- 完整上传后，14 项 GitHub 资产的名称、大小、SHA-256 与本地逐一核对，再公开发布；发布后复核全部 14 项，并回读公开 SHA256SUMS 和 release-manifest.json，字节一致。

## 完整产物

| 文件 | 字节 | SHA-256 |
| --- | ---: | --- |
| `drivebridge-0.2.32-darwin-arm64-feishu-desktop-unsigned-test.dmg` | 46910898 | `98ecf1658591f9fa8c3d6e3b1174b76cfe696f6f4cc34215b616a02b6b8bb237` |
| `drivebridge-0.2.32-darwin-arm64-feishu-desktop-unsigned-test.pkg` | 50948469 | `958c763db2a40d337d80337fa2665314df35b1f2e5de539ffb9698360fe707e1` |
| `drivebridge-0.2.32-darwin-arm64-feishu-desktop-unsigned-test.tar.gz` | 42145691 | `153d99ea450eae0859fb3c4f9f620b8b1187fcfef26f1b5a96a94864356115c6` |
| `drivebridge-0.2.32-windows-amd64-feishu-desktop-unsigned-test.exe` | 106424763 | `b213400857c7db5e9bbe926ef9b83fca661998b01e04786e69ed9d8bdb0a17d1` |
| `drivebridge-0.2.32-windows-amd64-feishu-desktop-unsigned-test.msi` | 143208448 | `c9104bfa4f9d1b2f88099ff24665c35468d7330c2130524cd50c61f9b99048a4` |
| `drivebridge-0.2.32-windows-amd64-feishu-desktop-unsigned-test.zip` | 136554042 | `2000dfd27a9a005f5860db0de79824cd092acc72372d3f6b437424d87ababa13` |

Published (UTC): `2026-11-23T12:01:43Z`.

附件另含两端 payload / artifacts / verification 清单、release-manifest.json、SHA256SUMS，共 14 项。原始载荷的未验证标记保留，自动化原生检查与当前用户 Mac 升级证据独立记录。

## 安装及边界

- macOS arm64：需要 macOS 14+、macFUSE；首选 DMG 图形安装器，PKG 通过配套 upgrade.sh 协调安装。
- Windows x64：需要 WinFsp、WebView2；EXE/MSI 通过 ZIP 中的 upgrade.ps1 和 PowerShell 7+ 协调安装，具体命令见发布页；ZIP 不自动注册右键入口。
- 未签名、macOS 未公证。不发布 Intel Mac / Linux 安装包。
- Windows 实机安装器升级、卸载、人工编辑器验收及真实账号故障注入独立于合成提供方原生门禁，尚未据此宣称完成。
- 本次发布不再次改动现场应用、配置、缓存或真实云端文件。
