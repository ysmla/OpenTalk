// Client-side mirrors of the server rules. They only give immediate feedback:
// the server stays the authority for every accepted value.

export const NICKNAME_MAX = 10;
export const BIO_MAX = 300;
export const PASSWORD_MIN = 8;
export const PASSWORD_MAX = 16;

const NICKNAME_PATTERN = /^\p{L}+$/u;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// Count Unicode code points, not UTF-16 code units, so astral characters and
// emoji count as one character instead of two.
export function charCount(value: string): number {
 return Array.from(value).length;
}

export function emailProblem(value: string): string | undefined {
 const email = value.trim();
 if (!email) return '请填写邮箱。';
 if (!EMAIL_PATTERN.test(email)) return '邮箱格式不正确，请检查后重试。';
 return undefined;
}

export function nicknameProblem(value: string): string | undefined {
 if (!value) return '请填写昵称。';
 if (!NICKNAME_PATTERN.test(value)) return '昵称只能由汉字或字母组成，不能包含空格、数字或符号。';
 if (charCount(value) > NICKNAME_MAX) return `昵称最多 ${NICKNAME_MAX} 个字符。`;
 return undefined;
}

export function passwordProblem(value: string, label = '密码'): string | undefined {
 if (!value) return `请填写${label}。`;
 const length = charCount(value);
 if (length < PASSWORD_MIN || length > PASSWORD_MAX) return `${label}长度需为 ${PASSWORD_MIN}–${PASSWORD_MAX} 个字符。`;
 if (!/\p{L}/u.test(value)) return `${label}需至少包含一个字母。`;
 if (!/[0-9]/.test(value)) return `${label}需至少包含一个数字。`;
 return undefined;
}

export function bioProblem(value: string): string | undefined {
 if (charCount(value) > BIO_MAX) return `简介最多 ${BIO_MAX} 个字符。`;
 return undefined;
}

export function emailCodeProblem(value: string): string | undefined {
 if (!value.trim()) return '请填写邮件验证码。';
 return undefined;
}

export function captchaAnswerProblem(value: string): string | undefined {
 if (!value.trim()) return '请填写图形验证码。';
 return undefined;
}

// 内容侧的限制，与服务端 server/internal/markdown 及内容接口的规则一致：
// 标题为单行纯文本，正文是 Markdown 原文，标签每篇最多 5 个。
export const TITLE_MAX = 120;
export const BODY_MAX = 20000;
export const TAGS_MAX = 5;

/** The server trims a title before storing it, so the count must trim too. */
export function titleProblem(value: string): string | undefined {
 const title = value.trim();
 if (!title) return '请填写标题。';
 if (charCount(title) > TITLE_MAX) return `标题最多 ${TITLE_MAX} 个字符（当前 ${charCount(title)}）。`;
 for (const character of title) {
  const code = character.codePointAt(0) ?? 0;
  if (code < 0x20 || code === 0x7f) return '标题必须是单行纯文本，不能包含换行或控制字符。';
 }
 return undefined;
}

/** The server stores the body unchanged, so only emptiness looks at the trim. */
export function bodyProblem(value: string): string | undefined {
 if (!value.trim()) return '请填写正文。';
 if (charCount(value) > BODY_MAX) return `正文最多 ${BODY_MAX} 个字符（当前 ${charCount(value)}）。`;
 return undefined;
}

export function tagCountProblem(count: number): string | undefined {
 if (count > TAGS_MAX) return `每篇最多选择 ${TAGS_MAX} 个标签（当前 ${count} 个）。`;
 return undefined;
}
