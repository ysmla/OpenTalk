# 备份恢复演练（2026-10-15）

## 结论

备份与恢复**真的跑过一次**：把有真实内容的夹具库 `opentalk_browser` 用 `mysqldump` 导出、恢复到临时库 `opentalk_restore`，逐表比对行数，并把应用连到恢复库上确认它**不只是行数一致、而是可以直接使用**。

这是第 9 项里我先做这一件的原因：它可验证、失败代价最高，而且最容易被评审质疑"你们真的演练过吗"。

## 演练步骤与结果

| 步骤 | 结果 |
| --- | --- |
| 导出（`--single-transaction --routines --triggers --events`，**不加 `--databases`**） | 37,911 字节；检查确认 dump 里**没有** `USE \`opentalk_browser\`` 语句（有的话恢复会写回源库） |
| 恢复到临时库 | 无错误 |
| 表数量 | 24 / 24 |
| 关键表行数（源 / 恢复） | contents 53/53、comments 48/48、topics 4/4、questions 1/1、answers 1/1、users 2/2、sessions 9/9、notifications 2/2、reports 1/1、moderation_actions 6/6、user_restrictions 1/1、deleted_content_archives 2/2、tags 2/2、topic_tags 1/1、outbox_events 0/0、email_deliveries 0/0 —— **不一致数：0** |
| 迁移状态 | `schema_migrations` 版本 4 / 4（恢复后应用仍认为 schema 是当前的） |
| **应用可用性** | 用恢复库启动 API：`/health=200`、`/ready=200`（就绪探针要求 schema 版本正确）、公开主题列表返回 3 条 |

## 演练过程中修掉的一处工具问题

第一次导出失败（退出码 1），而容器里直接跑同样的 `mysqldump` 是成功的。原因是 PowerShell 的 `Start-Process` 会把**单个字符串参数**重新拆分，于是 `sh -c "mysqldump …"` 被拆成了 `sh -c mysqldump`。改为不经 shell、把参数逐个传给 `docker exec` 后正常。

这条与前面几次编码/引号问题同类：**报错信息指向产品时，先确认调用方式本身没有把命令弄坏**。同时我保留了"容器内直接跑成功"这一步作为对照证据，否则很容易把它误判成 mysqldump 或数据库的问题。

## 第一次演练是"空库对空库"，我把它重做了

第一次我用开发库 `opentalk` 跑，结果所有表都是 `0 = 0`：**比对全部 OK，却什么也没证明**。于是改用有真实内容的 `opentalk_browser` 重跑，得到上面那张有非零数字的表。记下这一点是因为"演练通过"本身很容易自欺——**行数全为 0 的比对也是通过**。

## 未覆盖

- 未测**恢复耗时**：本机数据量很小（几十 KB），生产规模下的备份与恢复时长没有测量，因此也不能给出 RTO/RPO 数字。
- 未演练**部分恢复**（单表/单行找回）与**备份加密**；本文只在 `deployment.md` 里写明要求。
- 未在真实服务器上跑过（本机 Docker + 临时库）。
