# 导入 OpenTalk 数据模型到 Apifox

历史说明：用户已改为只维护本地文档，不再执行Apifox同步。模型已移动至 `../api/models.openapi.json`；完整接口合同见 `../api/openapi.json`，当前开发说明以 `../api/README.md` 为准。下方描述保留作为初次导入记录，其中待确认自答规则已由后续需求确认。

## 文件与范围

模型文件：`../api/models.openapi.json`。格式为 OpenAPI 3.0.3、UTF-8 JSON，包含 48 个命名模型和中文字段说明，所有引用均在文件内部。

本文件是接口请求/响应模型草案，不是 MySQL 建表文件。`paths` 为空，导入后不应出现新接口；尚未设计请求路径、HTTP 方法、鉴权绑定及状态码。导入不代表后端功能已实现。

## 导入步骤

1. 打开目标 Apifox 项目，进入「项目设置 → 导入数据」。
2. 如需再次手动导入模型，选择 OpenAPI/Swagger，上传 `../api/models.openapi.json`。
3. 选择目标分支与模块。已有项目建议使用可视化维护方式，在导入预览中检查数据模型。
4. 预期导入 48 个命名模型、0 个接口。如存在同名模型，先查看差异再决定更新或跳过，不启用删除未出现在本文件中的已有数据。
5. 完成后，在「数据模型」中查看 `QuestionTopic`、`Comment`、`ContentAuthor` 和 `DeletedContent` 等模型。

如果预览没有显示模型，请先取消导入并反馈预览结果，不要为了导入模型创建占位业务接口。当前未实际执行 Apifox 导入，工具端只支持读取；本地检查不能替代导入端验证。

参考：[Apifox 导入 OpenAPI/Swagger](https://docs.apifox.com/import-openapi-swagger)、[数据模型说明](https://docs.apifox.com/data-schemas)、[OpenAPI 3.0.3](https://spec.openapis.org/oas/v3.0.3.html)。

## 模型分组

| 分组 | 模型 |
|---|---|
| 用户与作者 | PublicUser、CurrentUser、NamedAuthor、AnonymousAuthor、ContentAuthor |
| 内容与展示 | Board、Tag、Attachment、DiscussionTopic、QuestionTopic、Answer、Comment、DeletedContent、PublicContent、ContentPermissions、ContentPage |
| 内容请求 | CreateTopicRequest、CreateAnswerRequest、CreateCommentRequest、EditContentRequest、SetResolutionRequest、DeletionResult |
| 注册与登录 | CaptchaChallenge、SendEmailCodeRequest、EmailCodeChallenge、RegisterRequest、LoginRequest、ResetPasswordRequest、UpdateProfileRequest |
| 通知与订阅 | NotificationPreferences、TopicSubscription、Notification |
| 用户举报 | CreateReportRequest、MyReport |
| 管理专用 | AdminUser、AdminAnonymousTrace、AdminTraceRequest、AdminDeletedArchive、AdminReport、AdminResolveReportRequest、AdminRestriction、AdminCreateRestrictionRequest、AdminAuditEntry、AdminSaveBoardRequest、AdminSaveTagRequest、AdminModerateTopicRequest、AdminSetVisibilityRequest |
| 通用错误 | ApiError |

## 与数据库模型的对应

- users → PublicUser / CurrentUser / AdminUser，按权限投影；密码摘要和会话秘密不出现在响应中。
- contents + topics + questions → DiscussionTopic / QuestionTopic；contents + answers → Answer；contents + comments → Comment。
- anonymous_identities → AnonymousAuthor，只暴露讨论内编号与展示名称，不输出真实映射。
- 删除后的 contents → DeletedContent；deleted_content_archives → AdminDeletedArchive，仅审核专用接口可引用。
- boards / tags / attachments → Board / Tag / Attachment；附件不公开真实存储路径或上传者内部ID。
- notifications / notification_preferences / topic_subscriptions → Notification / NotificationPreferences / TopicSubscription。
- reports → MyReport / AdminReport；user_restrictions / moderation_actions → 管理专用模型。
- sessions、outbox_events、email_deliveries 以及各关联表属于后端内部实现，不逐表暴露成公开接口模型。

## 已落实的规则

- ID 使用字符串，避免浏览器大整数精度丢失；时间使用带时区的日期时间字符串。可选字段不存在时省略，不传 null。
- NamedAuthor 与 AnonymousAuthor 通过 oneOf 分离，对象默认禁止额外字段；匿名响应不能混入用户ID、邮箱、头像或个人资料。
- 匿名帖子/问题作者的所有评论、回复必须由服务端强制匿名并显示“楼主”；其他参与者仍可逐条选择匿名。
- 已解决问题禁止任何人新增回答、评论、回复，包括回答下互动；重新打开才可按权限恢复。
- 删除问题连带全部回答和评论；删除帖子或回答连带下属评论；单独删除评论沿真实回复关系递归删除后代，不影响无关分支。
- 删除占位不携带原文、原标题、作者和图片；审核原文仅管理员专用操作可读，30天后清除。
- PublicContent 不引用管理员模型；ContentPermissions 是当前会话专用数据，不可作为公共缓存返回。

这些关联、状态和授权规则写入模型说明，但不能仅靠 JSON Schema 验证；后端必须在事务和权限检查中执行。

## 草案边界

- 后续已确认：匿名提问者自答强制匿名并显示楼主。
- 举报暂用 reason 文本，不预先锁定原因分类；落库 reason_code 的映射在接口定稿时确定。
- 邮件偏好默认值、长度上限、图片格式白名单、验证码长度/时限、密码复杂度未写成未经确认的硬约束。
- 头像清除等细节尚未设计；UpdateProfileRequest 目前只表达已提供字段的更新。
- 本次不包含收藏、点赞、采纳答案、私信模型。

## 本地验证

在仓库根目录执行 `python scripts/validate_apifox_models.py`（需要 Python 的 jsonschema 包）。

验证 JSON 可解析、内部引用完整、必填字段存在、模型可用 JSON Schema Draft 4 校验，并对匿名身份、删除占位与禁止客户端指定作者进行正反例检查。本文件使用的是与 Draft 4 兼容的 OpenAPI Schema 子集；这不是完整 OpenAPI 规范验证器，也不代替 Apifox 实际导入验证。
