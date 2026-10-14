# 图片隐私修复验证报告

日期：2026-10-14（Asia/Shanghai）。本报告回应 [2026-10-14测试报告](test-report-2026-10-14.md) 复现的三项隐私缺陷。三份缺陷均已在本机确认存在，修复后重新验证通过。

## 结论

测试报告中的三项失败全部属实，且根因与报告描述一致（不是环境或凭证问题）。修复已完成：

- 三个 `reviewprobe` 探针不再需要 build tag，已改成常规回归测试，随 `go test ./...` 一起执行。
- 修复后全量真实库测试：18个包、74个顶层测试、504个通过事件、0失败、0跳过；全包语句覆盖率 67.0%（修复前 66.6%）。
- `scripts/test-integration.ps1` 的 `-PrivacyProbes` 开关已删除，一次全量运行即覆盖这三项。

## 逐项确认与修复

### 1. 未发布图片允许游客读取 —— 确认并修复

复现：修复前 `go test -tags reviewprobe -run TestReviewPendingImageRequiresOwner` → `status=200; want 404`。

根因与报告一致：`attachmentReadable` 对 `pending` 行只判断期限，从未读取当前会话；`getImage` 也没有把会话传下去。存储键随机只降低猜中概率，不构成所有者检查。

修复：

- `attachmentReadableFor(ctx, key, viewerID)` 取代原来的无主体判断；`pending` 行要求 `viewerID != 0 && viewerID == owner_id` 且未过期。
- `GET /media/{key}` 每次请求用 `auth.RawFromRequest` + `Sessions.Lookup` 解析**可选**会话：无 Cookie、Cookie 格式不对、会话已撤销或已过期一律按游客处理并返回 404，不是 401——浏览图片不需要登录。
- 响应补 `Vary: Cookie`：判断结果现在依赖会话，共享缓存不能把作者看到的响应复用给游客。
- 旧测试 `attachments_test.go` 里"无 Cookie 读取 pending 必须 200"的错误断言已删除，改成四个类别的断言：作者 200、游客 404、其他**已登录**用户 404、过期后连作者也 404。

### 2. 隐藏根主题后回答图片仍可访问 —— 确认并修复

复现：修复前 → `status=200; want 404`。报告中"隐藏接口尚未实现、探针直接写入合法状态"的描述属实，本次沿用同样的方式（直接写库），因为审核界面仍未实现。

根因与报告一致：`active` 判断只看直接引用行的 `deleted_at` 与 `moderation_status`。隐藏根主题不会改写回答行的 `moderation_status`，所以回答关联的图片继续对游客开放。

修复：可见性改为沿祖先链解析——先定位引用该图片的内容行，再由它自己的 topic / answer / comment 关系求出**根主题**，要求引用行与根主题同时可见：

```sql
JOIN contents c ON c.id=ca.content_id
LEFT JOIN topics   t ON t.content_id=c.id
LEFT JOIN answers  a ON a.content_id=c.id
LEFT JOIN comments m ON m.content_id=c.id
JOIN contents root ON root.id=COALESCE(t.content_id,a.question_id,m.topic_id)
```

三张表里只会有一张命中，所以用 LEFT JOIN 加 COALESCE 求根。**这一点在实现时出过一次真错**：最初把 `topics` 写成内连接再依次 LEFT JOIN，结果回答行连接不到 topics，回答的图片在完全公开的讨论里也被 404。这个错误被新增测试当场抓住（`a visible question must serve its 回答 image: 404`），而不是被"更严格就更安全"的直觉掩盖过去。

新增断言：回答图与评论图在问题可见时对游客 200；隐藏根主题后两者对游客和其他登录用户都是 404；删除根主题后回答图立即 404；同一图片被多处引用时**任一可见引用即可读**，全部不可见才 404。

### 3. 上传图片元数据未剥离 —— 确认并修复

复现：修复前 → `privacy violation: uploaded JPEG metadata was stored unchanged`。报告中"代码直接写原始字节"的判断属实：`Save` 在 `inspect` 之后把 `data` 原样 `os.WriteFile`。

修复：`Save` 改为**解码后重新编码再落盘**，落盘的字节不再是上传的字节。

| 格式 | 处理 | 结果 |
|---|---|---|
| JPEG | 用标准库以质量88重编码 | COM/APP1/APP2 段（EXIF、GPS、XMP）全部消失 |
| PNG | 用标准库重编码 | `tEXt`/`iTXt`/`zTXt`/`eXIf`/`iCCP` 全部消失 |
| GIF | `gif.DecodeAll` + `gif.EncodeAll` | 注释扩展块消失；**帧数、每帧延迟、循环次数保留**（只重编码单帧会静默丢失动画） |
| WebP | `nativewebp` 无损重编码 | RIFF 容器里的 EXIF/XMP 块消失 |

EXIF 方向：剥离 orientation 标签会让手机竖拍照片在浏览器里转向，所以先解析 IFD0 的 tag 0x0112（大小端都支持），把 1..8 的旋转/镜像作用到像素上，再丢弃该标签。GIF 没有标准方向元数据，不处理。

新增依赖 `github.com/HugoSmits86/nativewebp`（纯 Go 的 WebP 无损编码器）。理由：`golang.org/x/image` 只有解码器，不重编码就无法移除 WebP 容器中的元数据；而 CGo 的 libwebp 绑定要求 Go 构建环境带 C 编译器，代价过大。

## 报告中的附加静态发现

- **尺寸上限在昂贵解码之前生效**：已修。`normalize` 先 `image.DecodeConfig` 读头部并判定宽高，再 `image.Decode` 解像素。新增 `TestSaveLimitsBeforeDecodingPixels`：一个只保留前半段、头部声明宽600的PNG，在 `MaxDimension=100` 下必须返回 `ErrTooWide`——只有头部判定先跑才会得到这个错误。
- **内容 JSON 未校验第一次 Decode 后已到 EOF**：未修，仍是已知缺口，已列入 `implementation-plan.md` 的未验证项（尾随垃圾字节不会被拒）。这是契约收紧问题，改动会影响所有写入接口的既有行为，留待单独确认。
- **WebP 无自动化样本**：已解决。`internal/media` 现在有独立测试，用 `nativewebp` 生成真实 WebP 样本并验证重编码后像素一致。
- **管理员CLI与配置加载覆盖较弱**：本次未处理，仍按原样列为风险。

## 本次引入的取舍（需要评审知悉）

1. **存储字节 ≠ 上传字节**，`ByteSize` 是重编码后的大小。旧测试"读回字节与上传完全一致"已按报告建议修正为"画面一致"（PNG/WebP 逐像素相等，JPEG 在容差内）。
2. **重编码可能变大**：极端输入（已高度优化的 JPEG）重编码后可能比原文件大。校验按上限的 1.25 倍执行，超出按 413 拒绝，避免把接近上限的上传变成磁盘上的无界文件。
3. **JPEG 有一次有损重编码**：这是移除 EXIF 的固有代价，已写进合同描述（`docs/api/openapi.json` 的 `uploadImage`）。
4. **动画 WebP 仍只保留首帧**：`golang.org/x/image` 的 WebP 解码器本来只解首帧，与修复前一致，不是本次引入的退化；但它意味着当前对动画 WebP 的支持只是"不报错"，没有真正保留动画。
5. **写盘过程改为先 0600 后 0644 并落在独立文件**，避免读者观察到半写的图片。

## 复核命令

```powershell
# 仓库根目录，需要 Docker 可用（读取已在运行的开发容器）
.\scripts\test-integration.ps1        # 全量测试；三项隐私断言已在其中
.\scripts\smoke-local.ps1             # 真实API进程 + Vite代理 + 未知media key 404

cd server
go test -count=1 -v ./internal/media
go test -count=1 -v ./internal/content -run 'TestImageUpload|TestImagePrivacy'
gofmt -l . ; go vet ./... ; go build ./...

cd ..\web ; npm run build
```

## 尚未复验的部分

本次没有做浏览器级图片回归：内容侧前端尚未实现，无法通过界面走一遍"上传后预览未发布图片→发布→隐藏"。修复的证据是接口级与存储级测试，不是界面观察。审核界面（隐藏/恢复）也仍未实现，因此第 2 项的修复只用直接写库的合法状态验证了读取端。

## 本机证据

- `.cache/review-go-tests.jsonl`（全量测试事件）
- `.cache/review-coverage.out`（覆盖率）
- `.cache/api-smoke.log`、`.cache/web-smoke.log`（进程烟雾检查）

均为本地生成物，不提交仓库。
