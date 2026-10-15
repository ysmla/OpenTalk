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
