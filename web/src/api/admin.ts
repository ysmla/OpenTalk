import { request } from './client';
import type { components } from './generated';

export type AdminSettings = components['schemas']['AdminSettings'];
export type AdminUpdateSettingsRequest = components['schemas']['AdminUpdateSettingsRequest'];
export type AdminSettingsField = keyof AdminUpdateSettingsRequest;

/**
 * Numeric site settings. The bounds mirror the OpenAPI schema and the server
 * rules: they only power immediate client-side feedback next to each input, the
 * server stays the authority for every accepted value.
 */
export const ADMIN_NUMERIC_BOUNDS = {
 captcha_ttl_seconds: { min: 60, max: 1800 },
 captcha_max_attempts: { min: 1, max: 10 },
 email_code_ttl_seconds: { min: 60, max: 3600 },
 email_code_max_attempts: { min: 1, max: 10 },
 email_code_resend_interval_seconds: { min: 10, max: 600 },
 email_code_email_hourly_limit: { min: 1, max: 100 },
 email_code_ip_hourly_limit: { min: 1, max: 1000 },
 login_captcha_failure_threshold: { min: 1, max: 50 },
 media_max_bytes: { min: 65536, max: 104857600 },
 media_max_dimension: { min: 256, max: 8192 },
 media_max_per_content: { min: 1, max: 30 },
 media_pending_ttl_hours: { min: 1, max: 720 },
 media_orphan_cleanup_days: { min: 1, max: 90 },
} as const;

/** The names above are the ones that carry a min/max pair in the API. */
export type AdminNumericField = keyof typeof ADMIN_NUMERIC_BOUNDS;

export const ADMIN_NUMERIC_FIELDS = Object.keys(ADMIN_NUMERIC_BOUNDS) as AdminNumericField[];

export const ADMIN_FIELD_LABELS: Record<AdminNumericField | 'smtp_port', string> = {
 smtp_port: 'SMTP 端口',
 captcha_ttl_seconds: '图形验证码有效期',
 captcha_max_attempts: '图形验证码最大尝试次数',
 email_code_ttl_seconds: '邮件验证码有效期',
 email_code_max_attempts: '邮件验证码最大校验次数',
 email_code_resend_interval_seconds: '邮件验证码重发间隔',
 email_code_email_hourly_limit: '每邮箱每小时发送上限',
 email_code_ip_hourly_limit: '每 IP 每小时发送上限',
 login_captcha_failure_threshold: '登录失败多少次后要求图形验证码',
 media_max_bytes: '单张图片字节上限',
 media_max_dimension: '单张图片像素上限',
 media_max_per_content: '每篇内容图片数量上限',
 media_pending_ttl_hours: '待用附件保留小时数',
 media_orphan_cleanup_days: '孤儿附件清理天数',
};

/** Only an administrator may read the site settings. */
export function getAdminSettings(signal?: AbortSignal) {
 return request<AdminSettings>('/api/v1/admin/settings', { signal });
}

/**
 * PATCH only the fields that actually changed. `reason` is required and lands
 * in the audit log; `smtp_password` is write-only, never echoed back and never
 * logged.
 */
export function updateAdminSettings(body: AdminUpdateSettingsRequest) {
 return request<AdminSettings>('/api/v1/admin/settings', { method: 'PATCH', body: JSON.stringify(body) });
}
