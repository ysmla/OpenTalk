# 项目状态复核与测试报告

日期：2026-10-14（Asia/Shanghai）。本报告基于本次重新读取的代码及重新执行的测试，不沿用上一轮结果。核查开始时工作区干净；本次只增加诊断测试、测试脚本及报告，未修改业务实现。

## 结论

现有功能测试、构建和合同校验通过，但新增隐私回归探针有3项失败。当前不能将“现有测试全绿”解读为图片隐私规则已满足；建议优先修复这些问题再推进内容前端。

## 当前实现范围

- 账号：验证码、注册、登录退出、密码重置、本人资料、管理员设置已存在后端与前端实现。
- 内容后端：板块读取、创建主题/问题、回答、评论回复、解决状态、编辑、删除、图片上传读取和清理已实现。
- 内容前端仍未实现；内容列表/详情等合同中仍为planned的操作不能算作已完成。
- 本地OpenAPI：63个模型、57个操作，其中23个implemented、34个planned。
- 数据库：23张业务表，3个up/down迁移；另有schema_migrations。
- Docker Engine 29.6.1本次可用。开发MySQL 8.4与Redis 8.2容器正在运行，历史dockerInference故障本次未复现。
- 搜索、举报、审核界面及通知/outbox执行等未实现项仍保持未完成状态。

## 重新执行的检查

| 检查 | 本次结果 | 说明 |
|---|---|---|
| Go全量测试，真实MySQL和Redis | PASS | 69个顶层测试、466个测试/子测试成功事件；0失败、0跳过。数字包含父测试，不能当作466个独立场景 |
| 迁移往返与约束 | PASS | migrations包实际访问MySQL，非解析替代 |
| 账号、内容集成测试 | PASS | 实际MySQL测试库；账号使用独立临时Redis、测试SMTP接收器，不发送真实邮件 |
| 全包语句覆盖率 | 66.6% | `-coverpkg=./...`；覆盖率不能证明业务断言正确；未包括独立进程烟雾检查 |
| go vet / go build / gofmt | PASS | 静态检查和构建成功，gofmt无未格式化文件 |
| React生产构建 | PASS | OpenAPI类型生成、TypeScript检查、Vite构建成功 |
| 核心模型及完整OpenAPI校验 | PASS | 50个核心模型，完整合同63个模型、57个操作 |
| SQL结构校验 | PASS | 23张业务表，3对迁移 |
| API与Vite真实进程烟雾检查 | PASS | API存活200、前端200、代理存活200；无效测试凭证使ready返回503，未实现GET /topics返回404 |
| 新增隐私探针 | FAIL（3项） | 失败是业务缺陷复现，不是Docker或凭证故障；见下文 |

本次没有重新执行浏览器点击流程、真实邮件投递、负载测试或race检测，不把历史浏览器验收视为本次证据。

隔离措施：测试库由现有testdb工具随机创建并清理；Redis另启临时容器，结束后删除，不使用正在运行的开发Redis第15号库。开发库、账号与SMTP配置均未改动。

## 已复现的问题

### P1：未发布图片允许游客读取

位置：`server/internal/content/attachments.go:139`；反映错误预期的旧测试位于 `attachments_test.go:104`。

上传图片后处于pending状态；`attachmentReadable`只检查期限，不检查当前用户。公开 `/media/{key}` 处理器也没有把当前会话用户传入授权判断。知道链接的游客可读取作者尚未发布的图片。随机URL减少猜中概率，但不等于所有者权限检查。

新增 `TestReviewPendingImageRequiresOwner`：作者上传后，不带Cookie读取URL。预期404，实际200。旧测试明确要求无Cookie读取pending图片为200，所以常规测试反而固化了错误权限。

建议：pending仅所有者可读；读取时解析可选会话，公开内容图片仍可供游客访问。调整旧断言并加入所有者、其他登录用户、游客、过期图片四类用例。

### P1：隐藏根问题后，回答图片仍可访问

位置：`server/internal/content/attachments.go:144`。

active图片授权只检查直接关联contents的deleted_at与moderation_status，不检查根主题、回答和评论祖先。隐藏问题不会自动将每个回答行的moderation_status也改掉，因此回答关联图片继续对游客开放。

新增 `TestReviewHiddenAncestorBlocksAnswerImage`：上传图片→创建问题→发布带图回答→将根问题设为hidden→游客读取图片。预期404，实际200。隐藏接口尚未实现，此探针直接在随机测试库写入该合法状态来验证读取端的既定权限规则。

建议：统一实现祖先可见性判断；附件只在至少一个完整可见的内容引用或其他明确授权成立时返回。增加回答、评论及多重引用的测试。

### P1：上传图片的元数据未剥离

位置：`server/internal/media/media.go:103`。

Save在校验图片后直接将收到的data写入磁盘。匿名用户图片可能继续携带定位、设备信息等元数据，这与已确认的匿名和图片处理需求不符。

新增 `TestReviewUploadStripsJPEGMetadata`：向有效JPEG加入合法COM元数据段，上传后读取存储文件。私密标记仍原样存在，测试失败。此测试验证元数据保留，不声称构造了完整GPS EXIF样本；代码直接写原始字节也说明没有任何元数据剥离流程。旧图片测试还要求返回字节与上传原文完全相同，需要同步修正。

建议：在保留产品所需图像行为的前提下实施元数据清理，分别覆盖PNG/JPEG/GIF/WebP；明确动图和EXIF方向处理，避免修复隐私后破坏图片方向或动画。

## 额外静态发现与测试缺口

- `media.Save`先调用inspect完整解码，再比较尺寸上限（media.go:90、117）。尺寸限制没有在昂贵像素解码之前生效；应先DecodeConfig并校验，再解码像素。本次未用超大图片进行资源耗尽测试，此项为代码审查发现。
- WebP专用样本仍没有自动化覆盖；现有媒体行为主要通过内容集成测试覆盖。
- 内容JSON解码没有验证第一次Decode后已到EOF，建议后续增加尾随JSON及垃圾字节的输入检查。
- 管理员CLI、配置加载的测试覆盖较弱；进程烟雾检查只验证启动和健康接口，不代替这些功能的回归测试。

## 本次新增/修正的测试工具

- `scripts/test-integration.ps1`：从已运行的本地开发容器读取测试凭证（不打印），创建独立Redis并运行全量测试；只清理本次创建的测试资源。
- `scripts/smoke-local.ps1`：原脚本已落后于应用，未设置必填APP_SECRET且依赖旧二进制。本次改为先构建当前代码并生成临时测试密钥，随后实际通过HTTP检查。
- 两个 `privacy_review_test.go` 使用 `reviewprobe` build tag；它们是明确已知失败的独立诊断集，不会被普通 `go test ./...` 自动执行。修复后应转入常规回归测试，不能因常规套件仍全绿而忽略它们。

复现命令（仓库根目录，Docker需要可用）：

```powershell
pwsh -NoProfile -File scripts/test-integration.ps1
# 当前预期返回非零，并在日志中复现三项隐私失败
pwsh -NoProfile -File scripts/test-integration.ps1 -PrivacyProbes
pwsh -NoProfile -File scripts/smoke-local.ps1
```

本机详细日志：`.cache/review-go-tests.jsonl`、`.cache/review-coverage.out`、`.cache/review-privacy-probes.txt`。这些为本地生成物，不提交仓库。

说明：第一次全量执行因PowerShell拆分未加引号的覆盖率路径产生额外的“包不存在”错误，实际测试场景均通过。修正参数后已重新完整执行，最终TEST_EXIT=0；上表以第二次有效结果为准。
