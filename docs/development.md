# 开发与验证

## 当前阶段

已建立React/Vite/TypeScript、Go标准库HTTP服务、MySQL迁移、Redis连接配置和本地OpenAPI合同，并已在真实MySQL上通过建表、约束与迁移往返验收。账号闭环已实现：图形验证码、邮箱验证码注册、登录退出、找回密码、本人资料与管理员站点设置，且这些接口都有真实MySQL+Redis的端到端验收测试。内容闭环已开始：板块读取与服务端预置板块命令已完成，发帖、问答、评论等尚未实现。无Docker时仍可检查编译、模型和中间件。

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

账号闭环已完成，内容阶段的决策也已落盘（见需求、权限与数据模型文档）。下一步实现内容闭环：板块、发帖与提问、回答与评论、匿名身份、解决状态、Markdown、图片与删除，重点验证越权与匿名信息泄露。真实SMTP服务商参数和页面品牌风格仍待确认，密钥不放进聊天或仓库。
