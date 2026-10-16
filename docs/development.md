# 开发与验证

## 当前阶段

已建立React/Vite/TypeScript、Go标准库HTTP服务、MySQL迁移、Redis连接配置和本地OpenAPI合同，并已在真实MySQL上通过建表、约束与迁移往返验收。账号闭环已实现：图形验证码、邮箱验证码注册、登录退出、找回密码、本人资料与管理员站点设置，且这些接口都有真实MySQL+Redis的端到端验收测试。内容闭环的**前后端读写两侧**也已实现并做过真实浏览器验证：板块、发帖与提问、回答、评论与回复、匿名身份、解决状态、编辑、连带删除与原文归档、图片上传与受控读取、清理任务，以及列表、详情、编辑与删除的界面。此外**通知闭环**（站内通知写入与读取、邮件偏好、主题订阅与签名退订、`cmd/worker` 投递，并已用真实 SMTP 真发一封并确认收到）、**公开内容搜索**（`GET /api/v1/search`、ngram 全文索引、结果页板块/标签筛选）与**举报与审核**（举报、我的举报、管理员队列与结案、内容隐藏/恢复、管理审计，且链路已在真实浏览器走通）也已完成。**尚未实现**：管理员删除他人内容、匿名追溯、用户封禁、板块/标签管理、用户列表（合同仍 planned），以及上线准备（部署、TLS、备份恢复、端到端验收）。当前数字口径见下方「文档口径」与 `implementation-plan.md` 的进度快照。无Docker时仍可检查编译、模型和中间件。

**2026-10-15 完整验收的 6 类前端缺口已修复并复验**：外链确认对话框、普通评论与回复的逐条匿名开关、评论作者的编辑入口、问题本身的澄清评论区、`/me` 邮件偏好与详情页单篇订阅设置、`#/unsubscribe` 退订确认页（GET 不写状态，点击后才 POST）。复核发现的"正文内站内链接被当作外链"回归也已修复：确认框现在只对**站外 origin** 弹出，同源链接（锚点、相对路径、同源绝对地址）直达。复验为真实 API + 真实 MySQL/Redis 上的浏览器 46/46，复核方自带的独立边界脚本 5/5；做法、夹具与结果见 [完整验收 6 类缺口的修复与复验](verification-2026-10-15-acceptance-repairs.md)（`scripts/verify-repairs.ps1` + `scripts/verify-fixes-browser.py`）。同一份文档还记录了两处**邮件口径的统一**：新账号默认"回答/回复开、评论关"（合同与注释已改为描述这个默认值），取消订阅改为删除该主题设置行、读回 `inherit`。

依赖版本锁定在server/go.mod、server/go.sum与web/package-lock.json。当前工具链验证为Go 1.26.4、Node 24.18.0。数据库开发基线MySQL 8.4；Redis开发镜像8.2-alpine。上线前固定实际镜像digest并复验，不把开发compose直接作为生产部署。

## 启动依赖

在仓库根目录复制 `.env.example` 为 `.env`，设置本地数据库密码。Compose读取.env；Go进程不会自动执行或读取.env文件，需要显式导入环境。

```powershell
Copy-Item .env.example .env
# 编辑.env中的本地密码后启动
docker compose up -d --wait
```

开发容器端口仅绑定127.0.0.1：MySQL 13306、Redis 16379。Redis只用于临时数据，开发镜像不配置公开网络认证；生产必须使用私有网络与认证。

## 启动后端

在仓库根目录设置当前PowerShell的环境变量（将密码替换为.env实际值，不要提交真实凭证）：

```powershell
$env:MYSQL_DSN='opentalk:YOUR_LOCAL_PASSWORD@tcp(127.0.0.1:13306)/opentalk?parseTime=true'
$env:REDIS_ADDR='127.0.0.1:16379'
$env:APP_ORIGIN='http://127.0.0.1:5173'
$env:HTTP_ADDR='127.0.0.1:8080'
$env:APP_SECRET='<base64编码的32字节以上密钥>'
$env:TRUST_PROXY='false'
Set-Location server
go run ./cmd/migrate -action up
go run ./cmd/api
```

- `/health`返回200只表示进程存活。
- `/ready`要求MySQL、Redis可用且schema_migrations版本等于 `migrations.Latest`（当前为4）、dirty为false；否则返回503。
- `APP_SECRET`必填，缺失或解码后不足32字节时进程拒绝启动，不会退回默认值。它派生出CSRF令牌密钥和SMTP密码加密密钥；轮换后既有CSRF令牌全部失效，已保存的SMTP密码也无法解密。
- 服务支持Ctrl+C/SIGTERM优雅关闭。请求日志只记录请求ID、方法、耗时；依赖失败另记一条错误日志，但都不含请求体、查询参数和凭证。
- 会话有效期30天且不滑动续期；退出登录只撤销当前会话，重置密码撤销该用户全部会话。
- `TRUST_PROXY` 默认为false：此时按IP限流统计的是直连对端地址。若前面有反向代理，对端就是代理本身，按IP的配额会退化成全站共享，因此只有确认代理会覆盖 `X-Forwarded-For` 时才把它设为true。直接暴露给客户端时绝不能开启，否则任何人都能伪造该头绕过按IP限流。

## 管理员引导

第一个管理员不能通过HTTP接口产生：注册需要邮箱验证码，而发验证码又需要已配置的SMTP。用命令行完成引导：

```powershell
Set-Location server
# 查看当前设置，SMTP密码只显示是否已配置
go run ./cmd/admin -action show
# 配置SMTP；密码用管道输入，避免进入命令历史
'smtp-password' | go run ./cmd/admin -action set-smtp -host 'smtp.example.com' -port 587 -security starttls -username 'mailer' -from 'no-reply@example.com' -from-name 'OpenTalk' -password-stdin -reason 'initial setup'
# 创建首个管理员，跳过邮箱验证
'admin-password' | go run ./cmd/admin -action create-admin -email 'admin@example.com' -nickname 'Admin'
# 或把已有账号提权
go run ./cmd/admin -action promote -email 'user@example.com'
# 清空SMTP配置：端口填错时用它可以撤回，管理接口不接受0端口
go run ./cmd/admin -action clear-smtp -reason 'misconfigured'
```

- `-security`只接受 `none`、`starttls`、`tls`。选择 `starttls` 时，如果服务器没有宣告STARTTLS会直接失败，不会退回明文；`none`加认证账号在非localhost主机会被标准库拒绝，这是有意的。
- 参数请写成独立的带引号的值（`-host 'smtp.example.com'`）。`-host=smtp.example.com` 这类写法在部分Windows shell下会被拆开，导致后续flag丢失。
- 命令行变更没有登录会话，因此不写 `moderation_actions` 审计，`updated_by` 保持NULL；需要审计时加 `-actor-email` 指定一个已注册管理员。
- `create-admin` 跳过邮箱验证，请提醒本人登录后立即改密。
- `clear-smtp` 会把主机、端口、账号、密码和发信人一起清空。用 `set-smtp` 只能设置端口而不能清除它，因为0不是合法端口；把0当作「清空」会让手误与有意为之无法区分。

## 启动前端

另开终端，在web目录执行：

```powershell
npm ci
npm run dev
```

访问 http://127.0.0.1:5173 。Vite将/api、/health、/ready、/media代理到后端，同源传Cookie。访问域名必须与APP_ORIGIN相同，不要混用localhost与127.0.0.1。可用DEV_API_TARGET调整开发代理地址。

## 检查

```powershell
# 仓库根目录
python scripts/build_openapi.py
python -m venv .cache/validation
.\.cache\validation\Scripts\python.exe -m pip install -r requirements-dev.txt
.\.cache\validation\Scripts\python.exe scripts/validate_apifox_models.py
.\.cache\validation\Scripts\python.exe scripts/validate_contract.py

# server目录
go test -v ./...
go vet ./...
go build ./...

# web目录
npm run build
```

真实MySQL与Redis的集成测试靠环境变量开启；两个都设置才会运行，否则明确SKIP：

```powershell
# server目录。测试账号需要CREATE DATABASE权限
$env:OPENTALK_TEST_MYSQL_DSN='root:ROOT_PASSWORD@tcp(127.0.0.1:13306)/?parseTime=true'
$env:OPENTALK_TEST_REDIS_ADDR='127.0.0.1:16379'
go test -count=1 -v ./migrations     # 建表、约束、up/down往返
go test -count=1 -v ./internal/auth  # 账号闭环：注册、登录、会话撤销、重放与限流
```

两套测试都只创建并删除自己随机命名的测试库，不会改动已有schema。`./internal/auth` 还会连接Redis的**第15号数据库并先清空它**，所以不要把它指向生产实例或正在使用的库；它另外在127.0.0.1上临时监听一个SMTP接收器，不会真的发信。

## 图片与删除内容的清理（手动执行，需外部调度）

清理**不是**API进程内的定时器，而是一个手动命令：

```powershell
cd D:\code\OpenTalk\server
go run ./cmd/admin -action cleanup
# 或先构建：go build -o ..\bin\cleanup.exe .\cmd\admin ; ..\bin\cleanup.exe -action cleanup
```

输出形如 `待用附件清除 0、孤儿附件清除 0、归档原文清理 0`。需要 `MYSQL_DSN`、`APP_SECRET`（用于解密站点设置里的清理窗口）与 `MEDIA_ROOT`（默认 `data/media`）。

**为什么不做成内置定时器**：清理是低频运维动作。放进API进程会让多实例部署时每个实例各跑一遍，进程重启也会打乱节奏；而把它做成可重复执行的命令，正好配合下面这条"失败可重试"的设计。代价是必须自己挂调度。

调度方式（任选其一）：

```powershell
# Windows 计划任务：每天 04:00 执行一次
schtasks /Create /TN OpenTalkCleanup /SC DAILY /ST 04:00 ^
  /TR "powershell -NoProfile -Command \"& { cd D:\code\OpenTalk\server; .\..\bin\cleanup.exe -action cleanup }\""
```

```cron
# Linux crontab：每天 04:00
0 4 * * * cd /srv/opentalk/server && MYSQL_DSN=... APP_SECRET=... MEDIA_ROOT=/srv/opentalk/data/media ./cleanup -action cleanup >> /var/log/opentalk-cleanup.log 2>&1
```

**失败可重试**：每个阶段先把候选行标成 `pending_delete` 并提交，再删文件，只有文件确实删除后才置为 `deleted`。进程中断或 `unlink` 失败都会让该行停在 `pending_delete`，下次运行会重新处理；文件本就不存在按成功处理。**失败不会被提前标记为完成**，因此重复执行是安全的。

**孤儿附件不会在7天时就被删**：只要还有未到期的删除归档引用该内容，图片就会保留到归档到期之后（30天）。归档存在的意义就是让管理员复核原始材料，提前删图会让复核落空。

## 文档口径

`implementation-plan.md` 的**进度快照表与数据基线**是"当前状态"的唯一口径来源：合同操作数（implemented / planned 与已实现路径数）、模型与业务表数、迁移组数与 `migrations.Latest`、全量测试的包数 / 顶层测试数 / 通过事件数与覆盖率都以那里为准；本文与 README 描述当前状态，与快照冲突时以快照为准。

**带日期的报告**（`test-report-*.md`、`privacy-reverification-*.md`、`verification-*.md`）记录的是**当日快照**：其中的测试数字、合同计数与"未实现"清单在当时为真，按历史保留、不回改，因此它们与当前基线不一致是预期的。判断某项功能现在是否可用，看合同（`docs/api/openapi.json` 的状态字段）与快照表，不要看某份当日报告的结论句。

## 已移除的重复内容（2026-10-15）

本文件原来还包含日期化的验收记录、图片隐私修复的缺陷与修复说明，以及一份"下一阶段"清单。这些内容在别的文档里已经有了，同一件事有两个口径迟早会互相矛盾，所以删掉，改由下列文档承担：

| 删除的内容 | 现在看哪里 |
| --- | --- |
| 本次验收记录（2026-10-14）、浏览器端到端验证（2026-10-14）、本次修复的验收（2026-10-14 / 2026-10-15） | 各批 `docs/verification-*.md` 与 `docs/privacy-reverification-*.md`（按日期选对应那一份） |
| 图片隐私修复（2026-10-14）的缺陷清单与逐条修复 | `docs/verification-2026-10-14-image-privacy-fix.md`、`docs/privacy-reverification-2026-10-14.md`、`docs/privacy-reverification-2026-10-15.md` |
| 下一阶段 | `docs/implementation-plan.md` 的进度快照表与「供外部评审」 |

本文件因此只剩两件事：**本地怎么跑起来**，以及**怎么看其它文档的当前状态**。
