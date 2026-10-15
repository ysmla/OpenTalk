import { request } from './client';
import type { components } from './generated';

export type MyReport = components['schemas']['MyReport'];
export type MyReportPage = components['schemas']['MyReportPage'];

/**
 * `POST /api/v1/reports` — 举报内容。
 *
 * 举报是主张而不是裁决：它不会隐藏或删除内容，同一用户对同一内容只能有一个未结案举报（重复提交返回 409）。
 */
export function createReport(body: { content_id: string; reason: string; details?: string }, signal?: AbortSignal) {
 return request<MyReport>('/api/v1/reports', { method: 'POST', body: JSON.stringify(body), signal });
}

/** `GET /api/v1/me/reports` — 仅本人可见的举报记录，按最新在前。 */
export function listMyReports(query: { cursor?: string } = {}, signal?: AbortSignal) {
 const search = new URLSearchParams();
 if (query.cursor !== undefined && query.cursor !== '') search.set('cursor', query.cursor);
 const suffix = search.toString() === '' ? '' : `?${search.toString()}`;
 return request<MyReportPage>(`/api/v1/me/reports${suffix}`, { signal });
}

export type AdminReport = components['schemas']['AdminReport'];
export type AdminReportPage = components['schemas']['AdminReportPage'];
export type AdminAuditEntry = components['schemas']['AdminAuditEntry'];
export type AdminAuditPage = components['schemas']['AdminAuditPage'];

/** `GET /api/v1/admin/reports` — 举报队列（只含未结案），仅管理员。 */
export function listAdminReports(query: { cursor?: string } = {}, signal?: AbortSignal) {
 const search = new URLSearchParams();
 if (query.cursor !== undefined && query.cursor !== '') search.set('cursor', query.cursor);
 const suffix = search.toString() === '' ? '' : `?${search.toString()}`;
 return request<AdminReportPage>(`/api/v1/admin/reports${suffix}`, { signal });
}

/**
 * `PATCH /api/v1/admin/reports/{report_id}` — 结案。
 *
 * 只接受 resolved 与 dismissed，且**不改动内容**：处置内容要另外调用 `setContentVisibility`。
 * 两者分开是刻意的，审计因此能说清管理员到底选了哪一种。
 */
export function resolveReport(reportId: string, body: { status: 'resolved' | 'dismissed'; resolution_note: string }) {
 return request<AdminReport>(`/api/v1/admin/reports/${encodeURIComponent(reportId)}`, {
  method: 'PATCH',
  body: JSON.stringify(body),
 });
}

/** `PATCH /api/v1/admin/contents/{content_id}/visibility` — 隐藏或恢复内容，响应 204。 */
export function setContentVisibility(contentId: string, body: { visibility: 'visible' | 'hidden'; reason: string }) {
 return request<void>(`/api/v1/admin/contents/${encodeURIComponent(contentId)}/visibility`, {
  method: 'PATCH',
  body: JSON.stringify(body),
 });
}

/** `GET /api/v1/admin/audit` — 管理审计（只追加），仅管理员。 */
export function listAudit(query: { cursor?: string } = {}, signal?: AbortSignal) {
 const search = new URLSearchParams();
 if (query.cursor !== undefined && query.cursor !== '') search.set('cursor', query.cursor);
 const suffix = search.toString() === '' ? '' : `?${search.toString()}`;
 return request<AdminAuditPage>(`/api/v1/admin/audit${suffix}`, { signal });
}
