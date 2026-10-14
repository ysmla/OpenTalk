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
