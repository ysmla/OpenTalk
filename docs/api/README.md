# 本地接口合同

从2026-10-14起以本地文件维护，不再以Apifox同步作为开发前置条件。

- `models.openapi.json`：50个核心接口数据模型，编辑入口。
- `../../scripts/build_openapi.py`：路由、鉴权、分页及附加模型的编辑入口。
- `openapi.json`：运行上述脚本生成的完整合同，63个模型、57个操作。不要直接编辑生成文件。
- `../../web/src/api/generated.d.ts`：由完整合同生成的TypeScript类型，禁止手改。

## 状态与约定

已实现：`GET /health`、`GET /ready`；账号闭环的 `/auth/captcha`、`/auth/captcha/{challenge_id}/image`、`/auth/email-codes`、`/auth/register`、`/auth/login`、`/auth/logout`、`/auth/reset-password`、`/auth/csrf`、`/me`（GET与PATCH）、`/admin/settings`（GET与PATCH）；内容闭环的 `/boards`（GET）、`/topics`（POST）与 `/answers`（POST）。其余操作均为planned，未实现路由返回404，不返回虚假成功数据。`x-implementation-status`是唯一状态来源，`validate_contract.py`会拒绝未列入其白名单就标成implemented的操作，避免文档先于代码宣称能力。

- 路径前缀 `/api/v1`，受控图片为 `/media/{key}`，健康检查不带前缀。
- Cookie会话：opentalk_session、HttpOnly、SameSite=Lax，生产环境Secure。所有写请求要求精确Origin，登录后的写请求另需X-CSRF-Token。
- CSRF令牌根据原始会话凭证和服务端密钥确定性派生，数据库只存摘要；登录时返回、页面刷新后读取 `/auth/csrf`，前端只放内存，多个标签页读取不轮换。
- 会话Cookie为 `opentalk_session`，有效期30天且不滑动续期；退出登录只撤销当前会话，重置密码撤销该用户全部会话。
- 管理路由在会话校验后还要检查role=admin；身份追溯及删除原文读取为独立审计操作。
- 响应直接返回模型，不再包一层data；错误统一为code/message/request_id。字段校验422，状态冲突409，权限不足403，不存在或隐藏404，依赖失败503。
- 所有BIGINT ID在JSON中为字符串；时间为UTC日期时间。可选字段省略，不传null。
- 列表为items/has_more/next_cursor，默认20、上限100；游标必须绑定过滤和排序条件，不能用偏移量伪装稳定游标。
- 排序采用确定的时间+ID顺序。活动排序可能因新回复改变位置，第一版不承诺跨页快照一致性；客户端按ID去重。
- 公开用户页面排除匿名内容。删除占位没有正文、作者或附件；隐藏内容不可通过子资源直接访问。
- 已解决问题和匿名楼主的最终规则见需求；服务端事务验证不能由Schema代替。
- 邮件退订是站内确认页POST到 `/subscriptions/unsubscribe`，校验范围受限签名令牌，不因邮件扫描器GET自动退订。
- 外链中间页是前端 `/out?url=...`，只允许http/https目标，展示真实域名并由用户点击继续；后端不抓取目标URL。

## 更新流程

1. 变更需求与权限文档。
2. 修改模型或构建脚本，执行 `python scripts/build_openapi.py`。
3. 执行 `python scripts/validate_apifox_models.py`（历史名称，现验证本地模型）与 `python scripts/validate_contract.py`。
4. 在web目录执行 `npm run build`，自动重新生成类型并检查编译。
5. 实现对应业务后更新该操作的implementation状态，并增加实际接口验收。

OpenAPI完整验证依赖仓库根目录requirements-dev.txt。Schema本地字段与隐私检查、SQL语法检查、真实MySQL执行验证分别记录，不混为同一个“通过”。
