# 六类验收修复复核（2026-10-15）

## 结论

原六类功能缺口已补齐，现有修复浏览器套件 **44/44 通过**。独立补充检查 **4/5 通过**，发现一处新回归：正文内的站内链接被当成外链拦截。完整验收仍有这一项待修复。

> **该回归已修复**：`destinationOf` 现在先比较 `url.origin` 与本站 origin，同源链接直接放行，只有站外目标才弹确认框；
> 用例改为在正文容器 `.topic-body` 内分别覆盖锚点、相对路径、同源绝对地址与站外地址，浏览器套件 **46/46 通过**；
> 本报告自带的 `scripts/recheck-repair-boundaries.py` 也重跑通过（**5/5**，含"Markdown 内片段链接直达"这条）。
> 复验记录见 [完整验收 6 类缺口的修复与复验](verification-2026-10-15-acceptance-repairs.md) 的「复核发现的回归及修复」一节。
> 该脚本按正文旧标签"主题列表"定位链接，已改为按"锚点站内"定位，断言未变。本报告其余结论与数字按当日快照保留。

核验基线：HEAD `89bbe25` 加本轮开始时已有的未提交修复。没有修改业务实现，保留用户工作区；新增边界复现脚本和本报告。

## 待修复：正文内站内链接不再直达（P2）

位置：`web/src/components/LinkConfirmDialog.tsx:68`—`:76`。

`destinationOf` 使用 `new URL(href, window.location.href)` 将相对链接与锚点解析为完整地址后，只判断是否为 HTTP/HTTPS，没有判断 `url.origin` 是否等于本站 origin。因此 `#/topics` 也会触发“即将离开本站”。同源绝对链接及相对路径从该实现上也受影响；本轮动态复现的是 `#/topics`。

真实浏览器复现：

1. 发布包含 `[主题列表](#/topics)` 的问题。
2. 点击 **正文 `.topic-body` 内** 的“主题列表”，而非面包屑导航。
3. 地址仍为 `http://127.0.0.1:15176/#/topic/1`，并出现一个离站确认对话框；预期直接进入 `/#/topics` 且没有对话框。

现有 `verify-fixes-browser.py:149` 使用全页面 `get_by_role('link', name='主题列表').first`，实际点击面包屑；面包屑未挂正文链接拦截器，所以那条测试通过不能证明正文内站内链接直达。

建议修复：解析目标 URL 后先判定 origin，同源 HTTP/HTTPS 链接直接放行，仅站外目标要求确认。回归测试限定在 Markdown 容器内，同时覆盖锚点、相对路径、同源绝对地址和站外地址。

## 已通过复核

- **外链提示**：站外链接展示真实域名，取消不离站，确认后离站。唯独站内链接存在上述回归。
- **匿名评论/回复**：开关存在，真实请求携带 `is_anonymous=true`，评论按匿名身份返回。
- **评论编辑**：作者可进入、保存并回到所属讨论；他人编辑被拒绝。
- **问题评论**：无回答时也能直接评论；解决后输入框关闭，重开后恢复。
- **邮件偏好与单篇订阅**：可读取、修改、刷新保持；全局关闭不阻止站内通知页面访问。
- **退订**：有效令牌进入确认页；无效/缺失令牌有提示。额外将订阅设为 enabled、全局三项偏好设为 true，再用未登录浏览器确认退订：GET 保持 enabled，POST 返回 204 后变为 disabled，三项全局偏好仍为 true。
- **普通用户身份**：独立检查 `/me` 返回 `role=user`；测试账号与管理员使用不同浏览器上下文。

额外退订检查避免了原测试先把订阅关闭后再退订、难以证明实际状态变化的不足。测试令牌只针对临时数据库，使用测试密钥签发，不访问真实邮箱。

## 本轮测试结果

| 检查 | 结果 |
| --- | --- |
| Go 全量真实库测试 | 134 个顶层测试通过，594 个测试/子测试通过事件，0 失败、0 跳过 |
| Go vet / build / gofmt | 通过 |
| 前端类型生成、TypeScript、Vite 构建 | 通过 |
| OpenAPI 与合同校验 | 通过：63 个完整模型、57 个操作、49 条路径、23 张业务表、4 组迁移 |
| 修复浏览器套件 | 44/44 通过 |
| 独立边界检查 | 4/5 通过；正文内站内链接失败 |
| git diff --check | 通过；现有文档有 CRLF 自动转换提示，无补丁空白错误 |

通过事件数包含父测试，不等于独立场景数。本轮没有重跑旧 37 项 HTTP 验收或旧 35 项浏览器脚本，不将历史结果当成本轮结果。

## 复现与清理

新增 `scripts/recheck-repair-boundaries.py`，先执行现有 `verify-fixes-browser.py`，再检查正文站内链接和真实退订状态变化。可交给隔离环境脚本的 Probe 参数运行，例如：

```powershell
pwsh -NoProfile -File scripts/verify-repairs.ps1 -ApiPort 18086 -WebPort 15176 -Probe D:/code/OpenTalk/scripts/recheck-repair-boundaries.py
```

本次实际使用 `.cache/repair-recheck.ps1` 副本，额外改为独立临时 Redis 和隐藏后台进程；复现用例本身相同。环境使用唯一测试数据库，浏览器运行结束后 API、Vite、测试数据库和临时 Redis 均已清理。

结果文件：`.cache/browser-fixes-verification/results.json`（44/44）及 `boundary-results.json`（4/5）；站内链接复现截图：`internal-link-recheck.png`。全量测试日志：`.cache/review-go-tests.jsonl`。这些临时产物可能被后续运行覆盖。

本轮不包含真实 SMTP 投递、生产部署、负载或全站无障碍验收。
