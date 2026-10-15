import { request } from './client';
import type { components } from './generated';

export type ContentPage = components['schemas']['ContentPage'];
export type PublicUser = components['schemas']['PublicUser'];

/** `GET /api/v1/me/contents` — 本人内容（含本人匿名帖，仍以匿名模式渲染）。 */
export function listMyContents(query: { cursor?: string } = {}, signal?: AbortSignal) {
 return request<ContentPage>(`/api/v1/me/contents${cursorSuffix(query.cursor)}`, { signal });
}

/** `GET /api/v1/users/{public_id}` — 公开资料，只含公开字段。 */
export function getPublicUser(publicId: string, signal?: AbortSignal) {
 return request<PublicUser>(`/api/v1/users/${encodeURIComponent(publicId)}`, { signal });
}

/** `GET /api/v1/users/{public_id}/contents` — 该用户的**公开**内容（服务端在 SQL 里排除匿名）。 */
export function listUserContents(publicId: string, query: { cursor?: string } = {}, signal?: AbortSignal) {
 return request<ContentPage>(`/api/v1/users/${encodeURIComponent(publicId)}/contents${cursorSuffix(query.cursor)}`, { signal });
}

function cursorSuffix(cursor?: string): string {
 return cursor === undefined || cursor === '' ? '' : `?cursor=${encodeURIComponent(cursor)}`;
}
