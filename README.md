# OpenTalk

面向小型社区的论坛，支持普通讨论、问答与可追溯匿名发言，完全由AI开发.

技术栈：React / Go / MySQL / Redis，部署到自有服务器。

当前已完成首版数据设计、接口合同和前后端工程基础，并在真实MySQL上通过建表、约束与迁移往返验收。账号闭环已实现：图形验证码、邮箱验证码注册、登录退出、找回密码、本人资料和管理员站点设置，并有真实MySQL+Redis的端到端验收测试。内容闭环**前后端读写两侧**均已实现：板块、发帖与提问、回答、评论与回复、匿名身份、解决状态、编辑、连带删除与30天原文归档、图片上传与受控读取（待用图片只有作者可读、附件可见性沿祖先链判定、上传后重新编码以剥离EXIF等元数据）、清理任务，以及主题列表、单条内容与删除占位、会话权限、回答列表、评论列表与标签列表。**内容侧前端已在真实浏览器上对真实API走查通过**（主题列表、主题详情、发帖提问、Markdown 预览、图片上传、回答、评论与两层回复、回答与评论的编辑删除、解决状态、删除确认、匿名与越权表现），走查中发现并修复的缺陷见各批验证报告。**通知闭环**已实现并做过真实投递验证：站内通知与 outbox 写入、通知读取接口、邮件偏好、主题订阅与签名退订，以及 `cmd/worker` 投递（`SKIP LOCKED` + 租约、同主题合并、每收件人小时上限、发送前复查可见性/身份/订阅）。**公开内容搜索**已实现：`GET /api/v1/search` 使用两个 ngram 全文索引（迁移 `000004_search_indexes`），界面为 `#/search`，支持板块与标签筛选。**举报与审核**已实现且界面链路已在真实浏览器走通：举报、`#/me/reports`「我的举报」、管理端 `#/admin/moderation`（举报队列、结案、内容隐藏/恢复、管理审计）；结案只记录对举报的决定，隐藏内容是另一个动作，两者都写审计。合同当前为 **57 implemented / 0 planned**（57 个操作、63 个模型、23 张业务表、4 组迁移）——契约里定义的操作已全部实现；完整口径见 [开发路线与决策记录](docs/implementation-plan.md) 的进度快照。上线相关的做法见 [部署与运维](docs/deployment.md)，本地可复现的端到端验收见 [验收报告](docs/verification-2026-10-15-acceptance.md)（`scripts/run-acceptance.ps1`，37 项检查）。

- [第一版需求与验收标准](docs/requirements.md)
- [权限矩阵](docs/permissions.md)
- [数据模型与字段字典](docs/data-model.md)
- [本地接口合同与维护说明](docs/api/README.md)
- [MySQL表结构与事务设计](docs/database-design.md)
- [启动方式与验证说明](docs/development.md)
- [开发路线与决策记录](docs/implementation-plan.md)
- [部署与运维](docs/deployment.md)
- [端到端验收](docs/verification-2026-10-15-acceptance.md)
- [备份恢复演练](docs/verification-2026-10-15-backup-restore-drill.md)
- [内容读取端点实现与验证](docs/verification-2026-10-15-content-reads.md)
- [内容侧前端浏览器走查](docs/verification-2026-10-15-content-frontend-browser.md)
- [完整验收 6 类缺口的修复与复验](docs/verification-2026-10-15-acceptance-repairs.md)
- [通知邮件投递（含真实投递验证）](docs/verification-2026-10-15-delivery.md)
- [公开内容搜索（含索引优化与筛选控件）](docs/verification-2026-10-15-search.md)
