# 部署与运维

本文是上线的操作手册：部署步骤、迁移、后台进程、备份与恢复、回滚，以及那些**必须保持的配置**。与开发环境相关的启动方式见 [development.md](development.md)；本文只讲怎么把它跑成一个真实站点。

## 0. 上线前必须确认的三件事

| 事项 | 要求 | 为什么 |
| --- | --- | --- |
| `TRUST_PROXY` | **没有反向代理时必须保持 `false`** | 按 IP 的限流依赖 `X-Forwarded-For`；在没有人覆盖这个头的链路上开启它，等于让客户端自己声明 IP，限流退化成全站配额 |
| `APP_SECRET` | 生产用独立随机值，不进仓库、不进聊天 | 它派生出会话、验证码摘要、设置密文与邮件退订令牌四把子密钥 |
| `APP_ORIGIN` | 真实站点地址（含 scheme） | 它同时是写请求的 Origin 校验依据与邮件里链接的前缀；与浏览器地址不一致会直接表现为 403 |

## 1. 构建

```bash
cd server && CGO_ENABLED=0 go build -trimpath -o ../bin/api ./cmd/api \
  && CGO_ENABLED=0 go build -trimpath -o ../bin/worker ./cmd/worker \
  && CGO_ENABLED=0 go build -trimpath -o ../bin/admin ./cmd/admin \
  && CGO_ENABLED=0 go build -trimpath -o ../bin/migrate ./cmd/migrate
cd ../web && npm ci && npm run build      # 产出 web/dist
```

三个二进制各有分工：`api` 是对外服务，`worker` 送通知邮件，`admin`/`migrate` 是运维命令。**`worker` 必须单独常驻**：它不在 API 进程里，API 只负责把事件写进 outbox。

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

1. 应用：保留上一个版本的二进制与 `web/dist`，切回即可（无状态）。
2. 数据库：迁移只前滚，回退业务数据走**备份恢复**，不要依赖 `down.sql`。
3. 通知邮件：把 `worker` 停掉即可——事件留在 outbox，恢复后继续投递，不会丢。

## 7. 上线后要盯的东西

- `/ready` 与错误率；`worker` 的每轮摘要（`claimed/sent/skipped/deferred/failed`）；
- `outbox_events` 里 `status='failed'` 的条数与最老的 `available_at`——它代表"有人注定收不到信"；
- `deleted_content_archives` 的 `purge_after`：清理任务需外部调度（`bin/admin -action cleanup`），**未执行不会导致原文被读取**（读取端自己按时间窗拒绝），但会一直占空间。

## 8. 未在本机验证的部分（不要当成已验证）

- 反向代理配置、证书签发与续期：本文只给要求，**没有在真实服务器上验证过**。
- 真实服务器上的 systemd/容器编排：未验证。
- 生产规模的备份时间与恢复时长：本机数据量很小，**没有测量**。
