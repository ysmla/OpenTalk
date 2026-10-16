# 完整验收 6 类缺口的修复与复验（2026-10-15）

## 结论

[完整验收报告](full-acceptance-2026-10-15.md) 复现的 **6 类前端缺口全部修复**，并在同一类隔离环境上重跑浏览器用例 **46/46 通过**（真实 API、真实 MySQL/Redis，无 mock）。[六类验收修复复核](acceptance-recheck-2026-10-15.md) 另外发现的"正文内站内链接被当成外链"回归也已修复并补上覆盖，其自带的独立边界脚本重跑 **5/5 通过**。复验时记录的两处"后端行为与文档不一致"已按确认的口径统一：**新账号邮件默认值以代码为准（回答/回复开）并改写合同与注释**，**取消订阅改为删除该主题的设置行（读回 `inherit`）**。两份报告都按"当日快照"原样保留，本文记录修复内容、复验方法与结果。

本轮**只改前端与验收脚本**，未改后端行为。复验中另外发现两处"后端行为与合同/注释不一致"，处理方式是**照实记录**（见下文「接口与文档不一致」），不改代码。

## 修复内容

| 报告编号 | 修复 | 主要文件 |
| --- | --- | --- |
| 1 外链无中间提示页 | 新增外链确认对话框：在渲染 Markdown 的正文里拦截左键点击，展示 URL 的**真实 hostname** 与「继续访问／取消」；站内链接、锚点、`mailto:` 不拦截。对话框挂在 App 上，主题、回答、评论、编辑器预览共用同一个 Markdown 入口，因此四处行为一致 | `web/src/components/LinkConfirmDialog.tsx`、`web/src/components/Markdown.tsx`、`web/src/App.tsx`、`web/src/style.css` |
| 2 普通评论与回复不能自主匿名 | 评论开关由 `CommentsSection` 持有并随每次提交发送（`is_anonymous: forceAnonymous \|\| anonymous`），`CommentComposer` 渲染它；匿名楼主由服务端强制匿名，开关为选中且禁用，并说明"展示为楼主，无法取消" | `web/src/views/CommentsSection.tsx`、`web/src/components/CommentThread.tsx` |
| 3 评论作者的编辑入口不可用 | 编辑器不再按内容 kind 拒绝评论（只拒绝删除占位）；新增 `topicIdOf()` 让保存后回到所属讨论，编辑页的"返回内容"链接同样指向讨论。越权与 30 分钟时限仍由服务端与权限接口决定 | `web/src/views/TopicEditorView.tsx`、`web/src/api/content.ts` |
| 4 问题本身没有澄清评论入口 | 问题页同时渲染主题级 `CommentsSection` 与 `AnswersSection`，两者共用同一个 `blocked`（锁定或已解决），因此已解决时问题评论、回答、回答下评论一起关闭 | `web/src/views/TopicDetailView.tsx` |
| 5 邮件偏好与单篇订阅未接入前端 | `/me` 增加三项邮件偏好表单（整体提交，只改选定项）；详情页增加单篇设置（跟随全局／始终发邮件／不发邮件，请求体 `topic_id` 由路径参数生成，不可能与路径不一致） | `web/src/components/MailPreferences.tsx`、`web/src/components/TopicSubscriptionControl.tsx`、`web/src/api/preferences.ts`、`web/src/views/ProfileView.tsx`、`web/src/views/TopicDetailView.tsx` |
| 6 邮件退订链接没有对应页面 | 新增 `#/unsubscribe` 路由与确认页：GET 只显示确认页、**从不写状态**，用户点击后才 POST；无效令牌由服务端拒绝并显示明确提示；缺少令牌时页面直接说明。页面按令牌 `key` 重建，换一个令牌或刷新到失效令牌不会沿用上一次的成功提示（这条是复验中发现的真实缺陷） | `web/src/views/UnsubscribeView.tsx`、`web/src/hooks/useHashRoute.ts`、`web/src/App.tsx` |

顺带修正的脚本问题：

- `scripts/run-acceptance.ps1` 用 `create-admin` 创建两个账号，其中"成员"实际是第二个管理员（报告第 28 行已指出）。现在创建后把成员降为 `role=user`，并在脚本内断言角色与账号数，角色不对就直接失败。
- `web/src/api/generated.d.ts` 由 `npm run build` 从当前合同重新生成，差异只有 23 行中文描述注释（`git diff` 已确认无类型变化）。

## 复核发现的回归及修复（2026-10-15，第二轮）

[六类验收修复复核](acceptance-recheck-2026-10-15.md) 用独立边界脚本发现：**正文内的站内链接被当成外链拦截**。这是修复 1 引入的回归，已修复。

- 原因：`destinationOf` 把 href 解析成完整 URL 后**只检查 protocol**，没有比较 origin。一个同源链接（`#/topics`、`/health`、`http://本站/#/topics`）解析出来也是 http/https，于是同样弹出"即将离开本站"。
- 修复：解析后先比 `url.origin === window.location.origin`，同源直接放行；只有站外目标才要求确认（`web/src/components/LinkConfirmDialog.tsx` 的 `destinationOf`）。判断依据从"链接长什么样"改成"目标在哪个 origin"，这正是 `requirements.md:76` 的两半——站内链接直达、外链经过中间页。
- 原用例为什么没抓到：`verify-fixes-browser.py` 当时用全页面 `get_by_role('link', name='主题列表').first` 点击，命中的是**面包屑**；面包屑不经过 Markdown 拦截器，所以那条断言通过。复核报告第 21 行已指出这一点。

修复后的用例把断言限定在正文容器 `.topic-body` 内，并覆盖四种形态：

| 用例 | 正文里的链接 | 期望 |
| --- | --- | --- |
| 正文锚点链接直达 | `[锚点站内](#/topics)` | 直接进入 `#/topics`，无对话框 |
| 正文相对路径直达 | `[相对站内](/health)` | 直接导航，无对话框 |
| 正文同源绝对地址直达 | `[绝对站内](http://本站/#/topics)` | 直接进入 `#/topics`，无对话框 |
| 外链未立即离开本站／中间页展示真实域名／取消后停留在帖子页／确认后才离开本站 | `[验收外链](https://example.test/outbound)` | 弹确认框、显示 `example.test`、取消不离站、确认后离站 |

浏览器套件因此从 44 项增加到 **46 项**（新增 3 项站内形态检查，原全页面的"站内链接直达"一条被这 3 条取代），修复后 **46/46 通过**。旧行为下这 3 条会失败：点击后出现对话框、点击事件被 `preventDefault`，`wait_for_url` 超时。这一点做过实测：把 `destinationOf` 临时改回"只检查 protocol"后，三条站内检查全部失败（`dialogs=1`，地址停在 `#/topic/1`），外链检查仍通过；改回修复版后 46/46。

复核报告自带的独立边界脚本 `scripts/recheck-repair-boundaries.py` 也重跑通过，**5/5**：

| 独立检查 | 结果 |
| --- | --- |
| 成员账号确实是 `role=user` | 通过 |
| 正文 Markdown 内的片段链接直达、不弹离站确认框 | 通过（`url=/#/topics`，`dialogs=0`）——就是复核报告里失败的那一条 |
| 打开退订页不改变 `enabled` 订阅 | 通过 |
| 未登录状态点击确认，把 `enabled` 变 `disabled` | 通过（204） |
| 退订不改变已开启的全局三项偏好 | 通过 |

改动复核脚本两处：① 它按正文里"主题列表"这个旧标签定位链接，而该标签随本轮正文内容一起改了，因此改为按"锚点站内"定位；② 它原来断言退订后读回 `disabled`，现在按"取消订阅＝删行"改为断言 `inherit`。两处断言语义未变（仍是"未登录也能通过邮件链接取消该主题订阅"）。

补记（第二轮决定之后）：该脚本与浏览器套件都已在新的退订语义下重跑，分别为 **5/5** 与 **46/46**。

## 复验

### 环境与夹具

`scripts/verify-repairs.ps1`：每次运行创建唯一命名的临时库 `opentalk_repair_<时间戳>`、两个一次性账号（`verify-member@example.test`、`verify-admin@example.test`）、临时 API 与临时 Vite（默认 18084／15174），跑完删除临时库并结束两个进程。不使用开发库与开发账号。

`scripts/verify-fixes-browser.py`：浏览器用例。退订的有效令牌由 `scripts/mint_unsubscribe_token.py` 按服务端格式现场签发（HKDF-SHA256 子密钥 + HMAC 载荷，与 `server/internal/content/subscriptions.go` 一致）。

### 浏览器：46/46

正文链接的四种形态（锚点／相对路径／同源绝对地址直达，站外地址弹确认框后取消或继续）、评论与回复的匿名开关按提交内容生效并返回匿名身份、评论编辑页可用且保存后回到所属讨论而他人被拒、问题在无回答时即可评论且已解决后无任何输入框、重新打开后恢复、邮件偏好按服务端值渲染并只改选定项且刷新保持、关闭全部邮件后站内通知页正常、单篇设置写入并保持、有效令牌点击后退订 204 且该主题读回 `inherit`（设置行被删除）、GET 打开退订页不改设置、无效令牌被服务端拒绝 400 且无残留写入、无未捕获浏览器异常。

### 后台回归

- `gofmt -l ./cmd ./internal` 无输出；`go vet ./...`、`go build ./...` 通过。
- 真实 MySQL + Redis 上 `go test ./...` 全包通过（`internal/content` 116.3s、`internal/auth` 14.1s、`migrations` 7.3s、其余包 0.03–4.9s）。
- `npm run build`（重新生成类型 + `tsc --noEmit` + vite）通过。
- `scripts/validate_contract.py`：57 operations / 49 paths、23 张业务表 / 4 组迁移通过；`scripts/validate_apifox_models.py`：50 schemas 通过。

### 复验过程中修正的两处夹具错误（不是产品缺陷）

1. **PowerShell 管道把同一个密码给了两个账号**：连续两条 `'密码' | & admin.exe ...` 让第二个进程读到的是第一个密码，于是"成员"登录后其实是管理员会话，一度表现为"实名评论被强制匿名"。改为每个账号单独传值，并断言两个账号都存在、成员角色为 `user`。
2. **成员页与管理员页共用浏览器上下文**：在同一个 Playwright context 里登录管理员会覆盖共享的会话 Cookie，成员后续的写请求带着管理员的会话和成员的 CSRF 令牌，于是稳定复现 `CSRF_REJECTED`。改为管理员用独立 context，问题消失。
3. 另外，用例最初在页面仍显示"正在加载…"时就断言，把加载中的空状态当成失败；`visit()` 现在等待 `main` 出现且页面上所有 `正在…` 占位消失后再断言。

## 接口与文档不一致：已按决定统一（2026-10-15 第二轮）

复验发现两处"后端行为与合同/注释不一致"。经确认后按下面的口径把**代码、合同、注释一起改到一致**，不再是"按现状记录"。

### 1. 新账号的邮件偏好默认值：以代码为准（回答/回复默认开）

- 决定：默认值保持代码现状——`DefaultEmailPreferences`（`server/internal/auth/users.go`）为**回答开、回复开、评论关**；合同与注释改成描述它，而不是把它说成"三项全关"。
- 改动：`scripts/build_openapi.py` 里 `GET /me/notification-preferences` 的描述改为"回复与回答默认开启、评论默认关闭，以账号里实际存的行为准；没有偏好行时才读作三项全 false"，并重新生成 `docs/api/openapi.json` 与 `web/src/api/generated.d.ts`（`npm run build`）；`server/internal/content/preferences.go` 与 `server/internal/auth/users.go` 的注释同步；`/me` 的邮件偏好表单补一句用户可见说明："新账号默认在收到回答与回复时发邮件、收到评论时不发，三项在这里都能改"。
- 现在的真实行为：**注册账号（走 `CreateUser`）第一次读取就是 `{评论关,回答开,回复开}`**，因此没打开过设置的用户会收到新回答与新回复的邮件；只有"完全没有偏好行"的账号（早于该默认值存在的账号）读作三项全关。这个区别写进了合同描述与两处代码注释。
- 测试为什么仍通过：`TestNotificationPreferencesDefaultToOff` 验证的是"没有行"这一分支（`internal/content` 的夹具直接插 `users`、不建偏好行），它现在与合同描述的分支一致；真实注册路径的默认值由 `auth` 包的测试覆盖。

### 2. 取消订阅：改为删行，读回 `inherit`

- 决定：退订（以及"取消该主题订阅"这一语义）应当**撤回该主题的选择**，而不是写入它的反面。
- 改动：新增 `Store.ClearTopicSubscription`（`DELETE FROM topic_subscriptions WHERE user_id=? AND topic_id=?`），`unsubscribeEmail` 改用它；重复提交幂等（204）。合同里 `POST /subscriptions/unsubscribe` 与 `GET /me/subscriptions/{topic_id}` 的描述同步改成"没有设置行时读作 inherit"、"成功后设置行被删除，该主题回到跟随全局"。
- 测试：`TestUnsubscribeTokenClearsOnlyItsTopic`（原 `…DisablesOnlyItsTopic`）改为先显式把该主题设为 `enabled`，再断言退订后读回 `inherit`、表里 0 行、相邻主题不受影响、全局偏好不变、二次提交仍 204；`unsubscribe_mail_test.go` 的真实报文取令牌用例同样改为断言 `inherit` + 0 行。`delivery_test.go` 里"显式 `disabled` 压过全局"的用例不变（显式关闭仍是合法选择）。

### 这两条决定合起来的一个后果（写清楚，避免误解）

取消订阅会删行，于是该主题回到跟随全局；而全局默认是"回答与回复开"。因此**一个全局开着回答/回复邮件的账号，点了某篇邮件里的退订链接后，仍可能继续收到该主题的回答邮件**——因为退订撤回的是"这一篇的单独设置"，不是"全局同意收邮件"。想彻底停邮件要在 `/me` 把全局三项关掉；退订成功后页面上就有"管理其他邮件偏好"的入口。以前写 `disabled` 时不会有这个效果（那一行会一直压着全局），这是本次决定带来的行为变化。

## 接口与文档不一致（原记录，2026-10-15 第一轮）

以下两条是最初的发现原文，保留用于追溯；结论已在上一节按决定统一。

### 1. 新账号的邮件偏好不是"三项全关"

- 当时的合同与代码注释：`GET /me/notification-preferences`「未设置过时三项均为 false：邮件是选择性开启的，不替用户发明默认值」，`server/internal/content/preferences.go` 的注释同义。
- 当时实际：`server/internal/auth/users.go:20-26` 定义 `defaultEmailOnComment=false`、`defaultEmailOnAnswer=true`、`defaultEmailOnReply=true`，`CreateUser` 在同一个事务里插入这样一行，而**注册接口正是走 `CreateUser`**（`server/internal/auth/endpoints.go:271`）。任何真实注册账号第一次读取就是"回答与回复已开"。
- 影响：投递前读到的就是这个默认值（`preferenceAllows` 直接取 `EmailOnAnswer`／`EmailOnReply`），所以**用户在从未打开过邮件设置的情况下就会收到新回答与新回复的邮件**。这与当时"不发明默认值"的说明方向相反。本次浏览器复验开始时的实测值就是 `{false,true,true}`。
- 为什么测试没抓到：`internal/content` 的夹具用自己的 SQL 直接插 `users`（`topics_test.go:106`），不建 `notification_preferences` 行，所以 `TestNotificationPreferencesDefaultToOff`（读作全 false）通过；它验证的是"没有行"这一分支，而真实账号永远有行。
- 处理：确认为有意选择，**代码保持默认开**，改合同与注释去描述它（见上一节）。

### 2. 邮件退订写入 `disabled` 行，而不是删行

- 当时的合同说明：「未设置过读作 inherit（跟随全局邮件偏好）」，枚举为 `inherit/enabled/disabled`。
- 当时实际：`unsubscribeEmail` 调 `SaveTopicSubscription(..., overrideDisabled)`，即写入 `disabled` 行。效果正确（该主题不再发邮件），但一个已经显式选过"这个主题不发邮件"的账号点击邮件退订后，`GET /me/subscriptions/{topic_id}` 仍然返回 `disabled` 而不是 `inherit`。复验中观察到的现象就是：退订 POST 返回 204，读回仍是 `disabled`。
- 处理：确认为应当"撤回该主题的选择"，**改为删行**，读回 `inherit`（见上一节）。

### 3. 退订页不在打开时预校验令牌（未改动，按设计保留）

服务端没有"只验证、不写入"的公开入口（`GET /me/subscriptions/{topic_id}` 需要登录，而退订页恰恰不要求登录），因此页面按设计在用户点击后才把令牌交给服务端校验。无效令牌此时得到明确提示（本例实测 400 +「这个退订链接无效或已过期。」），打开页面本身不写任何东西、也不显示成功。这与「失效令牌有明确提示」一致，只是提示发生在点击之后；若要"打开即提示"，需要新增一个不写状态的验证入口。

## 未覆盖

- 未做图形验证码人工识别、注册与找回密码的真实收件流程；未向外部邮箱发信，未重做生产 SMTP 投递验证。
- 未验证真实服务器上的 TLS、反向代理、备份恢复，未做压力/容量与完整无障碍测试。
- 本轮浏览器用例只覆盖上述修复相关的流程与页面，不等价于全站回归；报告「已验证的前端流程」一节列出的其余场景未在本轮重跑。
- 未跑报告原脚本 `scripts/browser-acceptance-review.py`（它需要 `acceptance.py` 的种子数据与 15174 端口的独立环境）。该脚本本轮未改动，其中的账号角色假设由 `run-acceptance.ps1` 的修正补上。
- 复核报告自带的 `scripts/recheck-repair-boundaries.py` 已重跑通过（5/5，见上），但它的退订检查用的是服务端签发的令牌，不覆盖"令牌过期"这一分支；过期令牌由服务端 `parseUnsubscribeToken` 在载荷内校验，属既有测试范围。
