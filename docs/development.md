# 开发与验证

## 当前阶段

已建立React/Vite/TypeScript、Go标准库HTTP服务、MySQL迁移、Redis连接配置和本地OpenAPI合同，并已在真实MySQL上通过建表、约束与迁移往返验收。账号闭环已实现：图形验证码、邮箱验证码注册、登录退出、找回密码、本人资料与管理员站点设置，且这些接口都有真实MySQL+Redis的端到端验收测试。内容闭环**后端**也已实现：板块、发帖与提问、回答、评论与回复、匿名身份、解决状态、编辑、连带删除与原文归档、图片上传与受控读取、清理任务；**内容侧前端尚未实现**。无Docker时仍可检查编译、模型和中间件。

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
- `/ready`要求MySQL、Redis可用且schema_migrations版本等于 `migrations.Latest`（当前为3）、dirty为false；否则返回503。
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

## 本次验收记录（2026-10-14）

Docker Desktop恢复正常（Engine 29.6.1、Compose v5.1.4），`docker compose up -d --wait` 启动的MySQL 8.4与Redis 8.2均healthy。以下检查已在真实数据库上执行并通过：

| 检查 | 命令 | 结果 |
|---|---|---|
| 迁移集成测试 | `go test -count=1 -v ./migrations` | PASS 2.9s：建出24张表、约束逐条拒绝非法数据、up/down往返、version=3且dirty=false |
| 账号接口端到端验收 | `go test -count=1 -v ./internal/auth` | PASS 9.5s：验证码单次使用与次数耗尽、用途绑定、注册与并发注册只建一个账号、登录、CSRF、跨站与越权拒绝、退出与重置撤销会话、重发与每小时限流、登录失败阈值、设置读写与审计 |
| 迁移应用 | `go run ./cmd/migrate -action up` | exit 0，`version=3 dirty=false` |
| 管理员CLI | `cmd/admin` 的 show / set-smtp / clear-smtp / create-admin / promote / seed-boards | 全部按预期；SMTP密码在库中为密文（用明文检索密文的结果为0）；`seed-boards` 幂等，重复执行不改动 |
| 真实发信 | 用管理员配置的阿里云邮件推送触发一次注册验证码 | SMTP事务成功、API返回202（约450ms），阿里云接受该邮件；密码从未出现在命令文本、日志或数据库明文中 |
| 板块读取 | `go test -count=1 -v ./internal/content` | PASS：空列表序列化为`[]`；游标绑定排序条件、跨接口游标被拒(400)；越界limit被拒(422)；归档板块仍可见 |
| Markdown写入校验 | `go test -count=1 -v ./internal/markdown` | PASS：围栏/缩进/行内代码里的`<script>`允许，正文里的原始HTML、注释、iframe拒绝；`javascript:`/`data:`/`vbscript:`/`file:`以及`java\tscript:`绕过写法全部拒绝；并有一组断言钉住解析器实际会把哪些目标变成链接 |
| 清理任务 | `go test -count=1 -v ./internal/content -run TestCleanup`，以及真实执行 `go run ./cmd/admin -action cleanup` | PASS：过期待用附件被清除且未过期的保留、**已关联图片即使被强制改成过期待用也不清除**、删除内容后孤儿附件须等窗口且**归档未到期时保留**、到期归档正文置空标题置NULL、**文件已缺失的待删除行按完成处理**、重复运行不再重复计数。开发库上真实执行两次均为全0且未改动任何数据 |
| 图片上传与读取 | `go test -count=1 -v ./internal/content -run TestImageUpload` | PASS：上传PNG写入pending行、**图片经 `/media/{key}` 由作者读回且画面与上传一致**（存储的是重编码结果，不是原始字节）、关联到内容后提升为active并清空delete_after、**删除内容后图片立即404**、非图片422、超像素422、未登录401、缺CSRF403、未知与穿越型key均404 |
| 图片隐私边界 | `go test -count=1 -v ./internal/content -run TestImagePrivacy` | PASS：待用图片只有作者200，**游客与其他登录用户都是404**；过期待用图片连作者也404；可见内容的图片对游客200；**隐藏根主题后回答与评论的图片同时404**（回答行自身的 moderation_status 仍是 visible，所以只有祖先判断能拦住它）；删除根主题后回答图片立即404；同一图片被多处引用时**任一可见引用即可读**，全部不可见后404 |
| 图片元数据剥离 | `go test -count=1 -v ./internal/media` | PASS：JPEG的COM段、PNG的tEXt块、GIF的注释扩展块在存储文件中都不再出现；GIF的帧数/延迟/循环保留；**PNG与WebP像素完全一致、JPEG在容差内一致**；EXIF方向1/3/6与非法值9的处理正确（6会交换宽高）；尺寸上限在像素解码之前生效 |
| 删除级联 | `go test -count=1 -v ./internal/content -run TestDelete` | PASS：帖子连带全部评论、问题连带全部回答与评论、回答只连带其评论、**删除评论递归只删回复而不动祖先与无关分支**、原文与原标题写入归档且`purge_after`恰好为删除时间+30天、活跃标题被替换、已删除内容立即可读性与互动均失效、非作者与管理员403、重复删除404、禁言仍可删自己的内容 |
| 内容编辑 | `go test -count=1 -v ./internal/content` | PASS：作者编辑正文并写入`edited_at`；非作者与**管理员**均403 NOT_CONTENT_OWNER；31分钟被拒409而29分钟仍可编辑（边界两侧都测）；**编辑不延长时限**；锁定主题409 TOPIC_LOCKED；**已解决不禁止编辑**；禁言403；已删除404；标题仅主题可改（回答422）；附件省略不改、空数组清空、他人附件拒绝；接口层401/CSRF_REJECTED、回答返回answer形状、主题返回discussion形状、危险正文422、未知字段400 |
| 解决状态切换 | `go test -count=1 -v ./internal/content` | PASS：提问者标记已解决并写入`resolved_at`；**切换后新增回答/主题评论/回答评论三者均被拒**，重新打开后恢复且`resolved_at`被清空；重复设置同一状态幂等；非提问者与**管理员**均返回403 NOT_TOPIC_OWNER且状态不变；讨论帖409、已删除404、非法取值422；**锁定后仍可重新打开**（solved不是单向门）；接口层401/CSRF_REJECTED/200返回整个QuestionTopic/未知字段400/无身份泄漏 |
| 评论与回复 | `go test -count=1 -v ./internal/content` | PASS：主题评论两个线程字段均为空且响应中省略、回答评论带answer_id、回复顶层评论时parent与reply_to相同、回复回复时parent保持线程根而reply_to为实际对象、**跨目标回复双向均422**、回复目标跨主题拒绝、回答跨主题拒绝、已解决问题禁止主题评论与回答评论、活跃时间被推进；匿名楼主评论强制匿名沿用编号、其他参与者为`匿名#1`；接口层401/CSRF_REJECTED/201契约形状/INVALID_COMMENT_TARGET/坏回复ID |
| 回答 | `go test -count=1 -v ./internal/content` | PASS：讨论帖拒绝回答(NOT_A_QUESTION)、锁定与已解决均为409（**提问者与管理员同样被拒**，重新打开后恢复）、已删除按404、answers行的question_id即主题ID、活跃时间被推进、禁言拒绝；匿名规则：匿名楼主自答传false仍按匿名存且显示楼主并沿用编号1、其他参与者为`匿名#1`且同人多次发言编号稳定、不同参与者递增、编号不跨讨论复用、非匿名主题作者带楼主标识；接口层401/CSRF_REJECTED/201契约形状与匿名不泄漏 |
| 发帖与提问 | `go test -count=1 -v ./internal/content` | PASS：真实MySQL上验证一个事务内的contents/topics/questions/匿名映射与回填、非匿名帖不建映射、讨论板块不建问题行、归档板块拒绝(409)、未知板块(404)、标签不存在与超限、重复ID去重、图片非本人/已作废拒绝、张数超上限、禁言拒绝且过期禁言放行、邮件覆盖只在显式设置时建行；接口层验证未登录401、缺CSRF与越权令牌均为CSRF_REJECTED、跨站Origin拒绝、原始HTML与危险链接422、超长标题422、未知字段400、问答返回question与open |
| Go全量测试 | `go test ./...` | 全部ok（account、auth、captcha、content、health、mailer、markdown、migrations、settings、secretbox） |
| 静态检查与构建 | `gofmt -l .`、`go vet ./...`、`go build ./...` | 无输出、干净、exit 0 |
| 前端构建 | `npm run build` | exit 0：类型生成、`tsc --noEmit`、vite build |
| 合同校验 | `validate_apifox_models.py`、`validate_contract.py` | PASS：50个模型、57个操作、23张业务表跨3个up/down迁移（迁移测试另计入schema_migrations，共24张）；已实现操作必须出现在校验脚本的白名单里 |
| 接口与代理联调 | 直连8080与经Vite 5173代理请求 | `/health`、`/ready`均200，未知路由404 |
| 管理员设置页（浏览器） | 真实API + 真实管理员/普通用户会话 | 管理员登录后个人中心出现「进入后台管理」，进入后表单加载真实SMTP与各项参数、密码字段为空；改一项并保存后数据库值更新、`updated_by`记为操作者、审计只记字段名（`LOCATE(改动值, reason)=0`）；普通用户看不到入口，直接访问 `#/admin` 只得到无权限提示且页面没有任何设置值 |

`/ready`返回200说明MySQL、Redis可用且schema_migrations版本等于 `migrations.Latest`、dirty为false，本次已满足。

Docker Desktop曾在本机创建/清理 `dockerInference` 运行时端点时退出，导致上述真实库检查一度SKIP；该故障在2026-10-14已不再复现。若再次出现同一报错，参考[Docker排障文档](https://docs.docker.com/desktop/troubleshoot-and-support/troubleshoot/)继续诊断，或改用单独的本地MySQL与Redis验证。无论环境如何，都不以SQL解析检查冒充建表成功。

## 浏览器端到端验证（2026-10-14）

除自动化测试外，账号闭环还在**真实浏览器**里跑过一遍：真实的Go进程、真实MySQL与Redis、以及一个只监听127.0.0.1的临时SMTP接收器（不是mock后端）。验证到的行为：

| 环节 | 观察结果 |
|---|---|
| 注册 | 图形验证码图片在页面上正确渲染；从Redis取该挑战的摘要反推出答案（与图片内容一致，两次分别解出 `E5YN`、`MUD2`）；提交后收到真实邮件并读出6位验证码；注册成功跳转登录页并带成功提示 |
| 落库 | `sessions` 计数为0（注册不建会话）；密码为 `$argon2id$...`，用明文检索哈希的结果为0；昵称与简介的字节与预期UTF-8完全一致 |
| 登录 | 会话Cookie带 `Max-Age=2592000`（30天）、`HttpOnly`、`SameSite=Lax`；本地http下正确地没有 `Secure` |
| 前端存储 | `document.cookie` 为空（会话Cookie是HttpOnly），localStorage与sessionStorage为空，URL中没有CSRF令牌——令牌只在内存 |
| 会话恢复 | 刷新页面后自动 `/me` + `/auth/csrf`，随后的一次 `PATCH /me` 成功写入，证明重新派生的CSRF令牌被服务端接受 |
| 退出登录 | `/me` 立即返回401，界面回到游客态；数据库里该会话是**撤销**（`revoked_at` 已置）而不是删除 |
| 登录失败阈值 | 5次失败后，第6次即使密码正确也被要求图形验证码；页面出现验证码块与友好提示，填入答案后登录成功，失败计数被清除 |
| 日志 | 应用日志只有 request_id、方法、耗时与依赖错误，没有URL、Cookie、请求体、密码或令牌 |
| Redis | 挑战、邮件码、限流、重发冷却、登录失败计数都有正的TTL，没有永不过期的键 |

这次验证用的验证码「解题器」是一个仅存在于系统临时目录的小工具（按同样的派生算法从摘要反推答案），**没有进入仓库**，也没有为测试削弱验证码难度。

## 下一阶段

**后端已完成账号闭环与内容闭环**：账号部分见上文；内容部分包含板块读取、发帖与提问、回答、评论与回复（含两层线程）、匿名身份与讨论内稳定编号、解决状态切换、编辑（30分钟时限）、连带删除与30天原文归档、图片上传与受控读取、以及清理任务。合同里标为 implemented 的操作清单见 [docs/api/README.md](api/README.md)，核对面与统计方法见 `implementation-plan.md` 的「供外部评审」一节。

接下来是**内容侧前端**：发帖页（Markdown 编辑与预览、图片上传）、内容渲染（markdown-it，必须禁用 HTML 并沿用服务端同一份链接白名单）、以及编辑、删除与解决状态的入口。到目前为止只有账号闭环与管理员设置页做过浏览器级验证，内容侧接口的真实浏览器验证要等这一步完成。

仍待确认或未实现的事项集中在 `implementation-plan.md` 的「供外部评审」一节。此前列为最高优先级的**图片 EXIF 元数据剥离已实现**（见下方「图片隐私修复」），不再是待办。

真实发信的 SMTP 服务商参数**已确认并配置**（阿里云邮件推送 `smtpdm.aliyun.com:465`，隐式TLS），且已真实投递验证过；仍待确认的是**页面品牌风格**。密钥只放在 `.env`（已被版本控制忽略）或通过管道传入，不放进聊天或仓库。

## 图片隐私修复（2026-10-14）

[2026-10-14测试报告](test-report-2026-10-14.md) 复现了三项隐私缺陷，本次全部修复；三个 `reviewprobe` 探针已改成常规测试，`scripts/test-integration.ps1` 不再需要 `-PrivacyProbes`。

| 缺陷 | 修复 | 现在的断言位置 |
|---|---|---|
| 未发布（pending）图片允许任何拿到链接的人读取 | `attachmentReadableFor` 对 pending 行同时要求"会话用户 == owner_id"且"未过期"；`GET /media/{key}` 每次请求解析可选会话（无会话/过期/伪造都按游客处理，不是错误），并补上 `Vary: Cookie` | `TestImagePrivacyBoundaries` 的前两个子测试 |
| 隐藏根主题后，回答与评论里的图片仍可访问 | 可见性判断改为沿祖先链解析：先定位引用该图片的内容行，再由它的 topic/answer/comment 关系求出根主题，根主题的 `deleted_at` 与 `moderation_status` 必须同时可见 | `TestImagePrivacyBoundaries` 的后三个子测试 |
| 上传图片原样落盘，EXIF/GPS 等元数据被保存 | `media.Save` 改为"解码 → 重新编码再落盘"。JPEG 用质量88重编码，PNG/WebP 用无附加块的编码器，GIF 用 `gif.DecodeAll`+`EncodeAll` 保留动画；EXIF 方向先应用到像素再丢弃，避免剥离元数据导致手机照片转向 | `TestUploadStripsImageMetadata`、`TestUploadAppliesExifOrientation` |

附带修正的代码审查项：尺寸上限现在由 `image.DecodeConfig` 在像素解码**之前**判定（`TestSaveLimitsBeforeDecodingPixels`），像素上限因此也无法被解压炸弹绕过。

新增依赖 `github.com/HugoSmits86/nativewebp`（纯 Go 的 WebP 无损编码器）：`golang.org/x/image` 只有解码器，不重编码就无法移除 WebP 容器里的元数据，而 CGo 版本的 libwebp 绑定会把 Go 工具链变成需要 C 编译器的构建。

**取舍记录**：存储的字节不再是上传的字节，`ByteSize` 是重编码后的大小，因此校验按同一上限的1.25倍执行，超出按413拒绝。JPEG 会经历一次有损重编码（这是移除 EXIF 的代价，已在合同里写明）。动画 WebP 仍只保留首帧——`golang.org/x/image` 的 WebP 解码器本来就只解首帧，这与修复前一致，不是本次引入的退化。

`scripts/smoke-local.ps1` 本次也修正为可在 Windows PowerShell 5.1（本机实际版本，没有 `pwsh`）上运行：密码改从开发容器读取而不是硬编码，`/ready` 现在断言真实的200，并增加"未知 media key 返回404"的检查。

## 本次修复的验收（2026-10-14）

```powershell
# 仓库根目录
.\scripts\test-integration.ps1     # 真实MySQL + 独立临时Redis
.\scripts\smoke-local.ps1          # 真实API进程 + Vite代理 + /media 404
cd server ; gofmt -l . ; go vet ./... ; go build ./...
cd ..\web ; npm run build
python scripts/build_openapi.py
.\.cache\validation\Scripts\python.exe scripts/validate_apifox_models.py
.\.cache\validation\Scripts\python.exe scripts/validate_contract.py
```

| 检查 | 结果 |
|---|---|
| Go 全量测试（真实 MySQL 与临时 Redis） | PASS：18个包、74个顶层测试、504个通过事件、0失败、0跳过 |
| 全包语句覆盖率 | 67.0%（`-coverpkg=./...`；覆盖率不证明业务断言正确） |
| gofmt / go vet / go build | 无输出 / 干净 / exit 0 |
| React 生产构建 | exit 0（类型生成、tsc、vite build） |
| 合同与SQL校验 | PASS：50个核心模型、57个操作、23张表3对迁移 |
| 进程与代理烟雾检查 | PASS：`/health` 200、前端 200、Vite代理 200、`/ready` **200**、未实现路由 404、未知media key 404 |

`/ready` 返回200说明MySQL、Redis可用且schema_migrations版本等于 `migrations.Latest`、dirty为false，本次已满足。此前该脚本硬编码了一个错误的数据库口令，因此只断言503；现在口令从开发容器读取，断言的是真实200。

Docker Desktop曾在本机创建/清理 `dockerInference` 运行时端点时退出，导致真实库检查一度SKIP；该故障在2026-10-14已不再复现。若再次出现同一报错，参考[Docker排障文档](https://docs.docker.com/desktop/troubleshoot-and-support/troubleshoot/)继续诊断，或改用单独的本地MySQL与Redis验证。无论环境如何，都不以SQL解析检查冒充建表成功。
