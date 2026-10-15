import { APIError } from '../api/client';

export type Failure = {
 /** Friendly, actionable text shown as the primary line. */
 text: string;
 /** The server's own message, when it adds context. */
 detail?: string;
};

// Codes worth their own Chinese copy plus a retry hint. Anything unknown falls
// back to the server message.
const FRIENDLY: Record<string, string> = {
 CAPTCHA_INVALID: '图形验证码不正确，请点击“换一张”获取新验证码后重试。',
 CAPTCHA_EXPIRED: '图形验证码已过期，请点击“换一张”获取新验证码后重试。',
 CAPTCHA_PURPOSE_MISMATCH: '图形验证码与当前操作不匹配，请点击“换一张”重新获取后重试。',
 CAPTCHA_REQUIRED: '登录失败次数较多，请填写图形验证码后重试。',
 EMAIL_CODE_INVALID: '邮件验证码不正确或已过期，请重新发送验证码后重试。',
 EMAIL_ALREADY_REGISTERED: '该邮箱已注册，请直接登录，或使用“找回密码”重置密码。',
 RATE_LIMITED: '操作过于频繁，请稍候再试。',
 INVALID_CREDENTIALS: '邮箱或密码不正确，请检查后重试。',
 ACCOUNT_BANNED: '该账号已被封禁，无法登录。如有疑问请联系管理员。',
 CSRF_REJECTED: '登录状态校验失败，请刷新页面后重新登录再试。',
 UNAUTHENTICATED: '登录状态已失效，请重新登录后再试。',
 // 内容侧的机器码，与服务端 server/internal/content 的机器码保持一致。
 TITLE_REQUIRED: '请填写标题。',
 TITLE_TOO_LONG: '标题超过 120 个字符，请缩短后重试。',
 TITLE_INVALID: '标题必须是单行纯文本，不能包含换行或控制字符。',
 BODY_REQUIRED: '请填写正文。',
 BODY_TOO_LONG: '正文超过 20000 个字符，请缩短后重试。',
 BODY_INVALID: '正文格式不符合要求，请检查后重试。',
 BODY_RAW_HTML: '正文不接受原始 HTML，请改用 Markdown 语法。',
 BODY_UNSAFE_LINK: '正文中的链接只允许 http、https 或 mailto，请修改后重试。',
 TOO_MANY_TAGS: '每篇最多选择 5 个标签，请减少后重试。',
 TAG_NOT_FOUND: '所选标签已不存在，请重新选择。',
 TOO_MANY_ATTACHMENTS: '关联的图片数量超过上限，请减少后重试。',
 ATTACHMENT_NOT_FOUND: '有图片已失效或不属于当前账号，请重新上传。',
 IMAGE_TOO_LARGE: '图片超过大小上限，请压缩后重新上传。',
 UNSUPPORTED_IMAGE: '只支持常见图片格式（如 JPEG、PNG、GIF、WebP）。',
 BOARD_NOT_FOUND: '所选板块不存在，请重新选择。',
 BOARD_ARCHIVED: '该板块已停止接收新主题，请选择其他板块。',
 CONTENT_NOT_FOUND: '内容不存在或不可见。',
 TOPIC_NOT_FOUND: '主题不存在或不可见。',
 TOPIC_LOCKED: '主题已被锁定，无法新增互动。',
 TOPIC_SOLVED: '问题已解决，不能新增回答、评论或回复。',
 NOT_CONTENT_OWNER: '只有作者本人可以修改或删除该内容。',
 NOT_TOPIC_OWNER: '只有提问者可以切换问题的解决状态。',
 NOT_A_QUESTION: '该主题不是问题，无法切换解决状态。',
 INVALID_COMMENT_TARGET: '回复对象无效，只能回复同一主题或同一回答下的评论。',
 MISSING_FILE: '请先选择要上传的图片。',
 INVALID_MULTIPART: '图片上传格式不正确，请重新选择文件。',
 PAYLOAD_TOO_LARGE: '请求内容过大，请减少正文长度或图片数量。',
 CURSOR_INVALID: '分页游标已失效，请重新加载列表。',
 // 内容写入的其余机器码（server/internal/content 的写入路径）。
 ACCOUNT_MUTED: '账号处于禁言期，暂时不能发布内容。',
 EDIT_WINDOW_CLOSED: '该内容的可编辑时间已过，无法再修改。',
 DEPENDENCY_UNAVAILABLE: '依赖的服务暂时不可用（如数据库或缓存），请稍后重试。',
};

function serverDetail(error: APIError): string | undefined {
 return error.serverMessage ? `服务器提示：${error.serverMessage}` : undefined;
}

export function describeError(error: unknown): Failure {
 if (!(error instanceof APIError)) return { text: '网络异常，暂时无法完成请求，请稍后重试。' };
 if (error.code === 'RATE_LIMITED' && error.retryAfterSeconds) {
  return { text: `操作过于频繁，请在 ${error.retryAfterSeconds} 秒后重试。`, detail: serverDetail(error) };
 }
 const friendly = FRIENDLY[error.code];
 if (friendly) return { text: friendly, detail: serverDetail(error) };
 return { text: error.serverMessage || '请求失败，请稍后重试。' };
}

/** Seconds the server asked us to wait, when it sent a Retry-After header. */
export function retryAfterSeconds(error: unknown): number | undefined {
 return error instanceof APIError ? error.retryAfterSeconds : undefined;
}

export function isStatus(error: unknown, status: number): boolean {
 return error instanceof APIError && error.status === status;
}

export function errorCode(error: unknown): string | undefined {
 return error instanceof APIError ? error.code : undefined;
}
