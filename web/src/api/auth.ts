import { request } from './client';
import type { components } from './generated';

export type CurrentUser = components['schemas']['CurrentUser'];
export type CaptchaChallenge = components['schemas']['CaptchaChallenge'];
export type EmailCodeChallenge = components['schemas']['EmailCodeChallenge'];
export type AuthSession = components['schemas']['AuthSession'];
export type RegisterRequest = components['schemas']['RegisterRequest'];
export type LoginRequest = components['schemas']['LoginRequest'];
export type ResetPasswordRequest = components['schemas']['ResetPasswordRequest'];
export type UpdateProfileRequest = components['schemas']['UpdateProfileRequest'];

export type CaptchaPurpose = 'register' | 'reset_password' | 'login';
export type EmailCodePurpose = 'register' | 'reset_password';

// Captcha challenges are single use and the image is served with no-store,
// so a fresh challenge_id is enough to bust any cache.
export function getCaptcha(purpose: CaptchaPurpose, signal?: AbortSignal) {
 return request<CaptchaChallenge>(`/api/v1/auth/captcha?purpose=${encodeURIComponent(purpose)}`, { signal });
}

export function sendEmailCode(body: components['schemas']['SendEmailCodeRequest']) {
 return request<EmailCodeChallenge>('/api/v1/auth/email-codes', { method: 'POST', body: JSON.stringify(body) });
}

export function register(body: RegisterRequest) {
 return request<void>('/api/v1/auth/register', { method: 'POST', body: JSON.stringify(body) });
}

export function login(body: LoginRequest) {
 return request<AuthSession>('/api/v1/auth/login', { method: 'POST', body: JSON.stringify(body) });
}

export function logout() {
 return request<void>('/api/v1/auth/logout', { method: 'POST' });
}

export function resetPassword(body: ResetPasswordRequest) {
 return request<void>('/api/v1/auth/reset-password', { method: 'POST', body: JSON.stringify(body) });
}

export function getCSRF() {
 return request<components['schemas']['CSRFToken']>('/api/v1/auth/csrf');
}

export function getMe(signal?: AbortSignal) {
 return request<CurrentUser>('/api/v1/me', { signal });
}

export function updateMe(body: UpdateProfileRequest) {
 return request<CurrentUser>('/api/v1/me', { method: 'PATCH', body: JSON.stringify(body) });
}
