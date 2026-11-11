# 个人与共享目录发现：三个方案及只读验证

2026-11-11。首批同一飞书企业内使用；按用户要求做轻量方案比较和真实接口验证。本次没有修改产品代码、云端内容或共享权限。报告区分“网页可见的全部一级入口”和“每个入口下面的整棵子目录树”。

## 结论

推荐继续方案 C：网页登录会话发现目录入口，官方 CLI 负责子目录枚举和文件读写。

本账号的个人目录 3 项、可见共享 3 项、隐藏共享 7 项均已与飞书网页对账；按资源 Token 去重为 12 个目录。方案 A 在共享入口发现上有明确遗漏，方案 B 不能独立列出未知名称的目录。

这只证明当前账号的网页一级目录清单与接口相符，不证明所有企业、所有账号、未来网页版本或整棵子目录树都已完整验证。浏览器独立登录、后台自动刷新尚未实现，本次复用了已登录的 Chrome 会话。

完整脱敏结果：[discovery-verification.json](evidence/2026-11-11/discovery-verification.json)。原始响应与临时探测程序保存在忽略的 `.superpowers/sdd/development/discovery-probe/` 等目录，不提交账号名称、资源 Token 或凭据。

## 三个方案

| 方案 | 实现 | 优点 | 本轮验证和结论 |
|---|---|---|---|
| A：官方根目录递归 | 用户身份获取根元数据；空 folder_token 列根；已知文件夹逐层分页 | 官方接口；完全后台；复用现有 CLI、锁和鉴权 | 根返回7项，其中3个文件夹、4个其他资源；个人目录3/3匹配，但共享目录仅1/10匹配，遗漏9个。不能单独满足需求 |
| B：官方根＋搜索 | 根目录加 Search v2，按 folder 类型、所有者等筛选并分页，归并检索到的 Token | 无网页会话；官方接口维护成本较低 | 空关键词 folder 查询在page-size=1和20、含mine和不含mine时均返回0；按已知名称能准确找到2个根接口遗漏的共享目录。可作补充检索，不能作完整目录库存 |
| C：网页发现＋官方 CLI | 独立网页登录会话分别获取个人、可见共享、隐藏共享入口；目录正文仍由官方 CLI 访问 | 当前样本覆盖三组入口；用户无需粘贴目录链接；复用现有同步引擎 | 三组清单与网页一致；分页分别为1/1/1、1/1/1、3/3/1，末页均终止且没有重复；12个唯一根均可由官方CLI列举。建议继续实现，但保留私有接口及会话生命周期边界 |

## 已执行的真实验证

官方请求使用已安装官方 CLI 1.0.99，通过产品 `Manager.Auth.ManagedRunner` 取得既有用户绑定、执行身份核验和凭据锁，没有读取或输出 access token。网页 GET 使用 Cua 操作原生 Chrome，保存服务器 JSON 响应后在本机解析；没有导出浏览器 Cookie。

| 检查 | 实际结果 |
|---|---|
| 网页与CLI身份关联 | 官方根元数据 user_id 与网页个人文件夹所有者 ID 匹配 |
| 个人文件夹 | 3项，has_more=false；与网页侧栏3项逐名匹配 |
| 可见共享文件夹 | 3项，has_more=false；与网页侧栏3项逐名匹配 |
| 隐藏共享文件夹 | 7项，has_more=false；打开网页“View Hidden Folders”后7项逐名匹配 |
| 资源去重 | 个人与共享有1个重复入口；3+3+7个分类成员，12个唯一目录Token |
| 个人根其他资源 | 网页4项与官方根返回的4项非文件夹Token集合一致 |
| 个人分页 | 每页1项，三页分别1、1、1，has_more为true、true、false；与一次返回3项集合一致 |
| 可见共享分页 | 每页1项，三页分别1、1、1，终止与集合检查通过 |
| 隐藏共享分页 | 使用替代排序组合后每页3项，三页3、3、1，无重复；与一次返回7项集合一致 |
| 目录内容访问 | 12个唯一根均由官方CLI成功列举；累计完整列举300个唯一文件夹，没有遇到列表权限错误 |
| 递归完整性 | 已发现737个唯一目录Token，仅访问300个，剩余437个因探测预算未访问；未将此记为整棵树完成 |

### 网页请求及分页注意事项

以下是当前租户实测路径，属于网页内部接口，不是飞书开放平台的稳定公共契约。域名必须由当前用户的已验证企业配置取得，不能在产品中写死本机租户。

| 来源 | 路径 | 本轮通过的参数组合 |
|---|---|---|
| 个人文件夹 | `/space/api/explorer/v3/my_space/folder/` | `asc=1, rank=5, length=1` |
| 个人根其他资源 | `/space/api/explorer/v3/my_space/obj/` | 默认参数，本轮4项 |
| 可见共享 | `/space/api/explorer/v2/share/folder/list/` | `hidden=0, asc=0, rank=3, length=1` |
| 隐藏共享 | `/space/api/explorer/v2/share/folder/list/` | `hidden=1, asc=1, rank=5, length=3` |

续页使用响应 `last_label`，并保持本轮对应请求参数。不能假定所有排序组合都具备相同分页行为，也不把 rank 数值解释成未经核实的排序名称。

负例必须保留：隐藏目录使用 `hidden=1, asc=0, rank=3, length=3` 时，第一页返回3项且has_more=true，按last_label续页却返回0项、has_more=false；同一游标将length改为50仍为0。它没有恢复已知存在的其余4项。不能照搬参考仓库的参数，或仅凭has_more=false宣称完整。后续改用 `asc=1, rank=5`，分别独立验证三页无重复、末页终止、集合等于7项基准。

这不是已确认的飞书上游缺陷；可能与未公开的参数语义有关。本次只确认两个参数组合在本账号的不同结果。

## 方案 C 的产品实现方向

1. 首次运行由App引导完成官方CLI用户授权和独立网页登录；它们是两套会话，不假定一次授权自动打通。逐用户验证企业和用户身份关联。
2. 网页模块仅执行只读目录发现，向现有管理层输出名称、资源Token、所属分类、分页状态和扫描时间；普通文件及文档读写继续由官方CLI执行。
3. 私有会话由App管理的浏览器上下文保存；不复用或提取用户日常Chrome的Cookie数据库，不向服务器上传网页会话。每个OS用户、飞书身份单独隔离；退出账号时关闭该上下文。
4. 平时在会话内直接后台发HTTP请求并解析JSON，不需要逐页点击或截图。可评估Playwright的`browserContext.request`等共享会话请求上下文；这属于后续实现选型，尚未验证独立浏览器与飞书SSO/无头模式的兼容性。
5. 在账号级虚拟根展示“个人目录”“共享目录”；隐藏共享仍纳入发现，可用界面标记其隐藏来源。相同Token的多分类入口复用规范身份和同步任务，不能重复上传同一资源。
6. 根入口发现与正文下载分离。默认先列入口，打开子目录时按需枚举；需要离线完整目录索引时，再启动可恢复的后台元数据扫描，展示扫描进度和未完成状态。
7. 会话过期时保留既有目录视图，标记需要登录；分页异常、响应结构变化、限频或读取失败时标记partial/stale，不清空缓存，也不将“未发现”直接当作删除并丢弃待上传内容。
8. 发现成功后用官方CLI核验目录访问能力。本轮12个根都通过，不代表未来每个网页入口都必然能由应用访问；失败需保留入口和原因。

建议输出模型携带每个来源的状态，而非只有一个含糊的complete布尔值：

```text
DiscoverySnapshot
  account_binding
  scanned_at
  personal:       {status, count, pages}
  shared_visible: {status, count, pages}
  shared_hidden:  {status, count, pages}
  entries:       [{resource_token, name, memberships}]
  descendants:   {visited, queued, complete}
```

来源状态至少区分complete、partial、stale、needs_login、unsupported。complete只描述本次来源的分页完成，不承诺所有“持链接可访问但不在网页列表”的资源都已被发现。

## curl 与浏览器各自的职责

curl当然可以发送相同HTTP请求，但必须具有相同的有效授权和请求参数。官方开放接口使用用户access token；网页内部接口使用网页登录会话，两者不能直接互换。

本轮官方路线由CLI发HTTP；网页路线通过当前电脑工具操作已登录Chrome，再保存JSON响应验证。浏览器逐页保存是本次验证手段，不是产品运行方案。正式原型应由自己的会话请求上下文直接请求API，浏览器窗口仅在首次登录、重新验证时显示。不能把本次浏览器验证描述成“curl已验证”或“长期后台自动刷新已通过”。

## 仍需补足的验收

- 独立App浏览器首次登录、系统重启后会话复用、失效后恢复；验证企业SSO/MFA及账号切换。
- 超过50个真实根目录、扫描期间新增/改名/隐藏/撤销共享，以及不同权限用户之间的对账。
- 全子目录扫描的断点、限频退避、取消恢复及最后未访问项归零；本轮300/737不能当作完整树验收。
- API响应结构变化或分页不一致时，显示未完成并保护现有挂载和本机待上传数据。
- macOS、Windows、Linux独立浏览器运行时的安装体积、依赖及后台行为。当前通过的是macOS已登录Chrome的只读接口样本。

## 一手依据

- [官方CLI Drive接口定义](https://github.com/larksuite/cli/blob/main/internal/registry/catalog/services/drive.json)：空folder_token、根目录分页及快捷方式的语义。
- [官方CLI Search实现](https://github.com/larksuite/cli/blob/main/shortcuts/drive/drive_search.go)：Search v2、folder筛选及分页字段。
- [网页接口参考实现](https://github.com/lc0609/feishu-batch-download/blob/main/content.js)：个人/共享入口路径线索；其单次length50不能替代本轮分页验证。
- [Playwright APIRequestContext](https://playwright.dev/docs/api/class-apirequestcontext)：浏览器上下文与请求上下文共享Cookie存储；支持会话内直接发API请求，不需逐页操作网页。
- [Playwright认证状态](https://playwright.dev/docs/auth)：独立上下文及认证状态复用；状态文件包含敏感凭据，不能提交仓库。
