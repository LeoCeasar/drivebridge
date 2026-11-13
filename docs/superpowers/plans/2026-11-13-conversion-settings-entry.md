# 后台替换规则管理入口

问题：本机已安装 0.2.15、用户级 JSON 已生成，但管理页无入口。直接补齐现有“账号与目录”页面，不增加本地编辑/保存步骤。

设计：常显“后台自动替换”卡片，展示原始字符串、目标（文本/换行/删除）、作用位置及匹配方式；可以新增、删除、编辑并保存。显示配置位置和“重启后生效”，不在保存时更换正在执行的不可变规则快照。默认两条规则也能修改目标但匹配边界保持保护。规则文件依旧可由用户编辑。

接口：Manager.ConversionRules(ctx) (ConversionRulesView,error)，Manager.SaveConversionRules(ctx, expectedRevision string, document json.RawMessage) (ConversionRulesView,error)。ConversionRulesView 包含 Path string、Revision string（磁盘内容 SHA256）、Document json.RawMessage（完整有效规则）、RestartRequired bool。GET/PUT /api/conversion-rules，PUT {revision,document}；已有会话/CSRF保护，非法配置返回400及校验原因，修订不符409，不覆盖用户更改。保存先纯解析验证，锁内再校验磁盘修订并原子私有替换；不调用Configure修改运行中规则。

- [x] 后端：纯解析/有效规则导出、私有修订保存、Manager入口、非法/并发/原文件保护测试。
- [x] 管理页：常显入口、可编辑规则行与新建/删除、目标换行不丢失、脏编辑不被轮询覆盖、保存与重启提示；接口鉴权/CSRF与JS行为测试。
- [ ] 相关模块完整测试及必要race/Windows编译；一次最终复核；打包替换本机并实测入口和初始规则，推送main。

验证：三个变更包完整测试及race通过（系统链接器有非致命警告），Windows amd64 编译通过，全部24个界面逻辑测试通过；一次最终复核未发现重要问题。本机安装及可见入口仍待执行。
