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
