# 部署与运维

本文是上线的操作手册：部署步骤、迁移、后台进程、备份与恢复、回滚，以及那些**必须保持的配置**。与开发环境相关的启动方式见 [development.md](development.md)；本文只讲怎么把它跑成一个真实站点。

## 0. 上线前必须确认的三件事

| 事项 | 要求 | 为什么 |
| --- | --- | --- |
| `TRUST_PROXY` | **没有反向代理时必须保持 `false`** | 按 IP 的限流依赖 `X-Forwarded-For`；在没有人覆盖这个头的链路上开启它，等于让客户端自己声明 IP，限流退化成全站配额 |
| `APP_SECRET` | 生产用独立随机值，不进仓库、不进聊天 | 它派生出会话、验证码摘要、设置密文与邮件退订令牌四把子密钥 |
| `APP_ORIGIN` | 真实站点地址（含 scheme） | 它同时是写请求的 Origin 校验依据与邮件里链接的前缀；与浏览器地址不一致会直接表现为 403 |

## 1. 构建

**在开发机构建，把产物传上去；不要在服务器上编译。** 仓库根目录：

```powershell
# 生产产物必须在 Linux 上运行，所以交叉编译（Windows 默认产出 .exe，传上去会 "cannot execute binary file"）。
# 项目是纯 Go、无 cgo，可以直接交叉编译。
$env:CGO_ENABLED='0'; $env:GOOS='linux'; $env:GOARCH='amd64'   # 服务器是 arm64 就改成 arm64
cd server
go build -trimpath -ldflags '-s -w' -o ..\dist-deploy\bin\api     ./cmd/api
go build -trimpath -ldflags '-s -w' -o ..\dist-deploy\bin\worker  ./cmd/worker
go build -trimpath -ldflags '-s -w' -o ..\dist-deploy\bin\admin   ./cmd/admin
go build -trimpath -ldflags '-s -w' -o ..\dist-deploy\bin\migrate ./cmd/migrate
cd ..\web
npm ci
npm run build                                  # 产出 web\dist
# 把前端与部署件一起放进发布包
xcopy /E /I /Y web\dist ..\dist-deploy\web
xcopy /E /I /Y deploy ..\dist-deploy\deploy
```

检查产物真的是 Linux 二进制（前四个字节是 `7F 45 4C 46`，即 ELF）：`deploy/release.ps1` 会在上传前替你检查，不通过就直接退出。

三个二进制各有分工：`api` 是对外服务，`worker` 送通知邮件，`admin`/`migrate` 是运维命令。**`worker` 必须单独常驻**：它不在 API 进程里，API 只负责把事件写进 outbox。

### 1b. 一键发布（推荐）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File deploy\release.ps1 `
  -Server 1.2.3.4 -Domain opentalk.example.com
```

它按顺序做：本地产物自检（含 ELF 校验）→ scp 到 `/tmp/opentalk-release-<时间戳>` → 服务端前置检查（env 文件、`opentalk` 用户、OpenResty/nginx、systemd）→ 装二进制到 `/srv/opentalk/releases/<时间戳>/bin` → 装前端 → 切换 `current` 符号链接 → 装 systemd 单元 → 装代理配置并 `-t` → 迁移 → 重启 → 清理 → 跑 `smoke.ps1`。任一步失败就停下，不会留下"API 在跑但页面 404"的半成品。

常用参数：`-User`（默认 root）、`-WebRoot`（默认 `/srv/opentalk`）、`-ProxyReload`（默认 `openresty`，用官方 nginx 就传 `nginx`）、`-ProxyConfDir`、`-DryRun`（只打印计划，不改服务器）、`-SkipMigrate`、`-SkipSmoke`。

回滚：`ssh` 上去把 `current` 指回上一个 releases 目录并重启 `opentalk-api`、`opentalk-worker` 即可（见第 6 节）。

## 2. 数据库迁移

```bash
./bin/migrate -action up        # 幂等；/ready 会校验 schema_migrations 版本等于 migrations.Latest
```

迁移只前滚；回滚用同目录的 `*.down.sql`，但**发布流程里不要用 down 回退业务数据**——正确做法是恢复备份（见第 5 节）。

## 3. 进程与探针

- `api`：绑定内网地址，由反向代理终止 TLS 并转发。探针 `GET /health`（存活）与 `GET /ready`（依赖就绪：MySQL、Redis 可用且 schema 版本正确）。
- `worker`：`-interval 30s` 常驻，或 `-once` 交给计划任务。`-action show` 打印解析后的 SMTP 配置（密码只显示"已配置"），`-dry-run` 只报队列深度。
- 两者都读同一份 `.env`/环境变量；`worker` 使用独立的小连接池，不和 API 抢连接。

## 4. TLS 与反向代理

TLS 在代理层终止。代理需要做三件事：转发到 `api`、**覆盖** `X-Forwarded-For`、保留 `Host`。只有做到这三件事之后，才可以把 `TRUST_PROXY` 设为 `true`。

邮件里的链接使用 `APP_ORIGIN`，因此它必须是外部可访问的 `https://` 地址。

### OpenResty 上的具体做法

仓库里的 `deploy/nginx/opentalk.conf` 只用了标准模块（`proxy_pass`、`try_files`、`ssl`），**不依赖任何 Lua 模块**，所以 OpenResty 与官方 nginx 都能直接用。与 nginx 的差异只有三处：

1. **站点文件位置**：OpenResty 一般没有 `sites-available/sites-enabled`，把配置放进 `conf.d` 并在 `nginx.conf` 的 `http{}` 里 `include conf.d/*.conf;`（默认通常已有）：

   ```bash
   sudo cp deploy/nginx/opentalk.conf /usr/local/openresty/nginx/conf/conf.d/opentalk.conf
   sudo sed -i 's/opentalk\.example\.com/你的域名/g' /usr/local/openresty/nginx/conf/conf.d/opentalk.conf
   sudo openresty -t && sudo systemctl reload openresty
   ```

2. **静态文件的读权限**：worker 进程以降权用户运行（Debian 常为 `www-data`，源码编译常为 `nobody`）。`/srv/opentalk/current` 是符号链接，整条路径都要可进入：

   ```bash
   ps -o user= -C openresty | sort -u          # 看清 worker 用哪个用户
   sudo chmod 755 /srv /srv/opentalk
   sudo chmod -R a+rX /srv/opentalk/releases
   ```

   漏掉这步的表现是 **403**（不是 404），这是最常见的坑。

3. **版本**：配置用了 `http2 on;` 新写法，需要 OpenResty ≥ 1.25.3.1。老版本把它改成 `listen 443 ssl http2;` 即可（`-t` 会明确报 `unknown directive "http2"`）。

**证书**用 webroot 模式，不碰现有配置。先在 80 端口的 server 里放行 ACME 目录（`location /.well-known/acme-challenge/ { root /var/www/certbot; }`），然后：

```bash
sudo mkdir -p /var/www/certbot
sudo certbot certonly --webroot -w /var/www/certbot -d 你的域名 \
  --deploy-hook "systemctl reload openresty"
```

不要装 `python3-certbot-nginx`：它假设标准 nginx 布局，在 OpenResty 上常报错。证书路径就是配置里写的 `/etc/letsencrypt/live/<域名>/`；续期由 certbot 的 timer 自动完成，`--deploy-hook` 负责续期后重载。

**静态文件与 SPA 回退是代理的职责**：Go API 只提供 JSON、`/media`、`/health`、`/ready`，**不服务 `web/dist`**。配置里 `root /srv/opentalk/current/web` + `try_files $uri $uri/ /index.html` 就是这件事；漏了它首页会 404，`deploy/smoke.ps1` 会直接报出来。

## 4b. 部署后的验证

```powershell
# 开发机（能解析到该域名）。带管理员账号时会额外做一次真实登录，证明 Origin 校验、Cookie 与 CSRF 都对。
powershell -NoProfile -ExecutionPolicy Bypass -File deploy\smoke.ps1 -Base https://你的域名 `
  -AdminEmail you@example.com -AdminPassword '...'
```

期望输出 `SMOKE_OK`。它检查：`/health`、`/ready`（MySQL/Redis/schema 版本）、首页与 SPA 回退、`/api` 同源转发、`/media` 未知 key 404、三个公开读接口、游客访问会话端点返回 401。**前端没被代理服务时它会明确失败**，这是最常见的部署错误。

冒烟脚本不点界面，所以**还必须用真实浏览器走一遍**：登录 → 发帖 → 评论 → 传图并确认显示 → 搜索 → 举报 → `/me` 三个页面。然后确认邮件真的能收到：配好 SMTP、让第二个账号回答一次，检查收件箱与 `journalctl -u opentalk-worker` 的每轮摘要。

## 5. 备份与恢复

**备份**（不锁表的一致性快照）：

```bash
mysqldump -uroot --single-transaction --routines --triggers --events opentalk > opentalk-$(date +%F).sql
```

要点：`--single-transaction` 让备份与应用并发而不锁表；**不要加 `--databases`**，加了这个 dump 会带上 `USE` 语句，恢复时可能写回源库；文件要落到**数据库主机之外**并加密，因为里面是用户数据。

**恢复**（先恢复到临时库核对，再切换）：

```bash
mysql -uroot -e "DROP DATABASE IF EXISTS opentalk_restore; CREATE DATABASE opentalk_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"
mysql -uroot opentalk_restore < opentalk-2026-10-15.sql
# 核对：表数量、关键表行数、schema_migrations 版本，并让应用连上去看 /ready 是否 200
```

**恢复演练已经真跑过**，结果见[演练报告](verification-2026-10-15-backup-restore-drill.md)：24 张表与 16 张关键表的行数完全一致、迁移版本保留，而且**恢复库能被应用直接使用**（`/ready=200`，公开列表正常返回）。

## 6. 回滚

1. 应用：`deploy/release.ps1` 每次发布都会把二进制与前端装进 `/srv/opentalk/releases/<时间戳>/`，并把 `current` 指向它。回滚就是换回上一个链接：

   ```bash
   ls -1 /srv/opentalk/releases            # 找上一个版本
   ln -sfn /srv/opentalk/releases/<上一个> /srv/opentalk/current
   systemctl restart opentalk-api opentalk-worker
   ```

   进程无状态，因此这一步就是全部；前端也随链接一起回到旧版本。
2. 数据库：迁移只前滚，回退业务数据走**备份恢复**，不要依赖 `down.sql`。
3. 通知邮件：把 `worker` 停掉即可——事件留在 outbox，恢复后继续投递，不会丢。

## 7. 上线后要盯的东西

- `/ready` 与错误率；`worker` 的每轮摘要（`claimed/sent/skipped/deferred/failed`）；
- `outbox_events` 里 `status='failed'` 的条数与最老的 `available_at`——它代表"有人注定收不到信"；
- `deleted_content_archives` 的 `purge_after`：清理任务需外部调度（`bin/admin -action cleanup`），**未执行不会导致原文被读取**（读取端自己按时间窗拒绝），但会一直占空间。

## 8. 未在本机验证的部分（不要当成已验证）

- 反向代理配置、证书签发与续期：`deploy/nginx/opentalk.conf` 与本文的 OpenResty 步骤**没有在真实服务器上跑过**；已在本机用等价代理验证过"前端由代理提供、`/api` 同源转发"这条链路能通过 `deploy/smoke.ps1`，但真实 OpenResty、DNS 与证书仍需你按第 4 节确认。
- `deploy/release.ps1` 的远端步骤没有在真实主机上执行过（只做了 `-DryRun` 与语法检查），第一次请开一个新终端逐行看输出。
- 真实服务器上的 systemd 编排：未验证（单元文件只做了语法与路径设计）。
- 生产规模的备份时间与恢复时长：本机数据量很小，**没有测量**。
- 生产 SMTP 投递：只在开发机真发过一封；发信域名、退信与配额未在生产验证。
