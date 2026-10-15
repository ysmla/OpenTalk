# MySQL 物理设计与事务合同

基线：MySQL 8.4 / InnoDB / utf8mb4。迁移为 `server/migrations/000001_initial.up.sql`（22张业务表）、`000002_account_settings.up.sql`（站点设置表）、`000003_media_settings.up.sql`（图片限制列）与 `000004_search_indexes.up.sql`（`contents.body_md` 与 `topics.title` 的 ngram 全文索引，不含建表）；迁移工具另建 `schema_migrations`，当前 `migrations.Latest = 4`。四个迁移均已在本机真实MySQL 8.4上通过建表、约束与回滚往返验证（见开发文档的验收记录），但这不等于生产迁移已获准发布。全文索引一节的取舍见[搜索验证报告](verification-2026-10-15-search.md)：把两个 `MATCH` 写成 `OR` 时索引不被使用（实测与扫描相当），因此查询采用"两个索引子查询 `UNION` 后 join 回 `contents`"的形状。

## 存储与标识

- 内部主键 BIGINT UNSIGNED；接口中用十进制字符串传递。用户公开ID为应用生成的随机UUID文本，与内部ID分开。
- 正文 MEDIUMTEXT，仅保存Markdown原文；标题 VARCHAR(240)。删除时将原文移到审核表，正文/标题置NULL。正文可见性不能只靠SQL列类型保证。
- 昵称 VARCHAR(80)，邮箱规范化结果 VARCHAR(254)；公开ID、邮箱分别唯一，昵称不唯一。
- 时间 DATETIME(6)，所有连接固定UTC；客户端显示本地时区。
- 规范化邮箱和标签使用二进制排序规则后的唯一索引；规范化算法在账号实现阶段统一，不能让不同入口各自处理。当前算法为去掉首尾空白后做Unicode小写折叠，且不合并Gmail点号或加号别名。
- 密码用 argon2id 哈希（内存64MiB、迭代3、并行2、16字节随机盐、32字节输出），以PHC字符串存入 `password_hash`。参数从存储值中读取，所以以后调整参数不会让已有密码失效；`VerifyPassword` 会拒绝参数越界的摘要而不是按默认值强行计算。
- 当前字段长度属于存储容量选择，不等同于最终产品输入限制；实现请求校验时必须小于等于存储容量。
- 图片放独立存储目录，表中只存随机storage_key、格式、大小和归属。读取附件需检查内容关联、删除、隐藏及审核权限。

## 相比逻辑草案的落实

| 决策 | 原因 |
|---|---|
| 用户状态从有效user_restrictions派生，不重复存users.account_status | 避免封禁到期后状态列与处罚记录不一致；API仍输出active/banned |
| sessions增加csrf_token_hash | 会话Cookie与CSRF令牌分别验证；均只保存摘要 |
| 举报采用reason文本 | 与当前接口模型一致，原因分类未确定前不虚构reason_code |
| anonymous_identity_id与author_id复合外键 | 防止内容绑定到另一用户的匿名映射；讨论范围仍由事务校验 |
| answers与comments使用复合外键 | 防止评论指向其他问题的回答或跨主题评论 |
| outbox/email任务增加lease_token | 超时重新领取后，旧工作进程不能凭过期租约确认任务 |
| notifications增加moderation_action_id | 管理通知引用审计，避免复制原文；只展示接收者有权看到的结果 |
| 删除归档增加deletion_root_id | 记录连带删除来源；重复删除不刷新已删除后代的期限 |
| 不使用物理ON DELETE CASCADE | 原文保留、软删除、审计和到期清理需要显式事务控制 |

表清单：users、sessions、boards、contents、topics、questions、answers、comments、anonymous_identities、tags、topic_tags、attachments、content_attachments、notification_preferences、topic_subscriptions、outbox_events、notifications、email_deliveries、reports、user_restrictions、moderation_actions、deleted_content_archives、settings。

## 关键约束

- 已解决必须有resolved_at，待解决必须没有resolved_at。
- contents删除时间与正文空值同步；不能只设置deleted_at却继续保存公开原文。
- 评论顶层的parent/reply_to同时为空，回复时同时非空，不允许自己回复自己。
- 同一主题的匿名用户映射和编号分别唯一。
- 举报active_slot生成列使同一人同一内容最多一个pending/processing举报，已结案历史可以保留多条。
- 审核purge_after必须等于首次deleted_at加30天。
- MySQL CHECK不能验证跨表业务流程；扩展类型、板块类型、回复同串/同回答、无环、匿名继承和权限均需要应用层验证。

## 站点设置与账号参数

`settings` 是单行表：主键列被CHECK约束固定为1，避免出现第二行让"当前设置"产生歧义。SMTP主机、端口、加密方式、账号、密码密文、发信人、显示名，以及图形验证码与邮件验证码的有效期、最大校验次数、重发间隔、邮箱/IP每小时上限和登录失败阈值都存这里；管理员后台修改后对后续请求立即生效，不需要重启进程。

- `smtp_password_ciphertext` 只保存应用层加密结果（AES-GCM，密钥来自环境变量），表中没有任何明文密码列；读取设置的接口和审计记录都不返回密码，只说明是否已配置。
- SMTP相关列可为NULL表示"未配置"；发信前必须确认主机、端口、发信人和密码都已配置，否则按依赖不可用处理，不能用空主机名去尝试连接。
- 数值列带下限CHECK，避免后台写入0或负数造成限流失效或验证码永不过期；重发间隔和有效期的上限同样受CHECK约束，避免一次配置把账号流程锁死。
- 修改设置写一条 `moderation_actions` 审计，`action` 为设置变更类型，`reason` 只记录改动了哪些字段名，不记录字段值，也不复制密钥。
- 验证码本身不进数据库：图形挑战与邮件验证码都放Redis，带TTL，图形验证码只存答案的HMAC和已渲染的PNG，邮件验证码只存验证码的HMAC，两者都不保存明文。
- 图片限制与清理时长与账号参数放在同一行设置里，由迁移000003加入：单张字节上限、像素上限、每篇张数、待用附件保留小时数、孤儿附件清理天数。每列都带NOT NULL默认值与上下限CHECK，既有行无需数据迁移，直接SQL也只能写入合法范围。

## 统一事务锁顺序

所有修改现有讨论的服务，先查得根topic_id，再开始事务并锁定 `topics.content_id`；随后读取根contents、questions及目标祖先的最新状态。不得在获得根锁之前缓存决定权限的状态。统一顺序为根主题→目标/后代content_id升序→附件→outbox。死锁仅有限次重试整个幂等事务。

### 创建回答、评论或回复

1. 校验当前会话、处罚状态及输入。根主题锁定后检查未删除、未隐藏、未锁定；问题必须open。
2. 校验回答归属、评论回复链和所有祖先可见性；从真实父节点推导展示根评论，拒绝跨回答与跨串引用。
3. 若当前用户是匿名主题作者，回答、评论、回复强制匿名并显示楼主；其他人保留逐条选择。复用该讨论匿名映射，必要时在根锁内分配编号。
4. 原子写contents与对应扩展行、图片关联、last_activity_at和outbox；提交后再发送通知。

新主题创建需要在同一事务中先建contents、topics，再建questions（如需）和匿名映射，最后回填anonymous_identity_id。提交前必须检查匿名映射已完整；允许临时NULL是为了解决外键循环，不代表允许发布不完整数据。

### 设置解决状态

根主题锁定后校验提问者身份及主题状态，更新resolution_status/resolved_at。已解决后禁止包括楼主在内的所有新增回答、评论与回复，已有内容保留。与创建操作共用根锁，确保并发提交有明确先后。

### 连带删除

1. 根锁内验证操作者可删除目标。问题选中全部回答和评论；帖子/回答选中其下评论；单条评论沿reply_to_comment_id遍历全部后代。
2. 对未删除记录保存归档（含原文、操作者、删除根、时间），清空公开正文/标题并设置deleted_at。已删除记录保持首次时间及归档期限。
3. 同事务记录内容失效事件；不再向这些对象新增互动。任何一步失败均回滚，不能分批提交出半删状态。
4. 读接口、搜索、附件、待发送通知统一检查祖先状态。公开通知不保留旧正文快照。

递归遍历必须处理超过MySQL默认CTE深度的讨论：可用根锁内逐层查询和去重集合，不能遇到深度限制后只删除已遍历的一部分。

### 30天清理与邮件

- 审核读取本身检查purge_after；清理任务延迟不能延长访问期限。
- 到期物理删除归档；附件仅在没有其他有效正文、头像或未到期审核引用时清除。删除文件失败可重试，不能提前把失败标记为已完成。
- outbox与业务写入同事务。工作进程按租约领取，确认任务时匹配lease_token；通知唯一键保证幂等。
- SMTP没有端到端恰好一次语义。发送成功后进程崩溃可能造成重发；使用稳定Message-ID和发送记录尽量去重，不能声称绝对不会重复。
- 邮件验证码继续存Redis；未注册邮箱的验证码邮件不使用要求recipient_id的业务通知表。若实现可靠队列，需为短期加密验证码任务单独设计存储，不将明文验证码放入outbox。
- 备份到期策略在上线前单独落实；业务库30天清理不自动保证历史备份即时清除。

## 执行与验证

`go run ./cmd/migrate -action up` 执行迁移，`-action version`查看版本。迁移库提供版本和dirty标记；DDL隐式提交，失败后须查清实际执行位置，不能直接force清除dirty再重试。

down文件仅供空白开发环境回滚验证。命令需显式 `-action down-one -allow-destructive`；禁止用它恢复线上误删。应用连接禁用多语句，只有迁移连接允许执行内嵌SQL多语句。

真实库测试见 `server/migrations/migrations_test.go`：创建随机 `opentalk_test_*` 数据库，测试建表、邮箱唯一、解决状态、匿名映射、举报去重、删除状态与保留期，并执行回滚再建表；最后只删除该随机测试库。没有测试DSN时明确SKIP。

依据：[MySQL外键](https://dev.mysql.com/doc/refman/8.4/en/create-table-foreign-keys.html)、[CHECK约束](https://dev.mysql.com/doc/refman/8.4/en/create-table-check-constraints.html)。
