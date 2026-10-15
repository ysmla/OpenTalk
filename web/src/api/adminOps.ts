import { request } from './client';
import type { components } from './generated';

export type AdminUser = components['schemas']['AdminUser'];
export type AdminUserPage = components['schemas']['AdminUserPage'];
export type AdminRestriction = components['schemas']['AdminRestriction'];
export type AdminRestrictionPage = components['schemas']['AdminRestrictionPage'];
export type Board = components['schemas']['Board'];
export type Tag = components['schemas']['Tag'];
export type PublicContent = components['schemas']['PublicContent'];
export type AdminDeletedArchive = components['schemas']['AdminDeletedArchive'];
export type AdminAnonymousTrace = components['schemas']['AdminAnonymousTrace'];

function cursorSuffix(cursor?: string): string {
 return cursor === undefined || cursor === '' ? '' : `?cursor=${encodeURIComponent(cursor)}`;
}

/** 用户管理列表。只含公开资料与当前是否被封禁，不含邮箱。 */
export function listAdminUsers(cursor?: string, signal?: AbortSignal) {
 return request<AdminUserPage>(`/api/v1/admin/users${cursorSuffix(cursor)}`, { signal });
}

/** 处罚记录（含已撤销的：撤销不等于删除）。 */
export function listRestrictions(cursor?: string, signal?: AbortSignal) {
 return request<AdminRestrictionPage>(`/api/v1/admin/restrictions${cursorSuffix(cursor)}`, { signal });
}

/** 创建禁言或封禁。封禁会同时撤销该账号的全部会话，因此立即生效。 */
export function createRestriction(body: { user_id: string; kind: 'mute' | 'ban'; reason: string; ends_at?: string }) {
 return request<AdminRestriction>('/api/v1/admin/restrictions', { method: 'POST', body: JSON.stringify(body) });
}

/** 撤销处罚：记录撤销时间，不删除记录。 */
export function revokeRestriction(restrictionId: string, reason?: string) {
 return request<void>(`/api/v1/admin/restrictions/${encodeURIComponent(restrictionId)}`, {
  method: 'DELETE',
  body: reason === undefined || reason === '' ? undefined : JSON.stringify({ reason }),
 });
}

/** 创建板块；`kind` 在该板块有主题之后就不能再改。 */
export function createBoard(body: { name: string; description: string; kind: 'discussion' | 'qa'; sort_order?: number; is_archived?: boolean }) {
 return request<Board>('/api/v1/admin/boards', { method: 'POST', body: JSON.stringify(body) });
}

/** 修改板块（整体替换；有主题时改 `kind` 会被服务端拒绝）。 */
export function updateBoard(boardId: string, body: { name: string; description: string; kind: 'discussion' | 'qa'; sort_order?: number; is_archived?: boolean }) {
 return request<Board>(`/api/v1/admin/boards/${encodeURIComponent(boardId)}`, { method: 'PUT', body: JSON.stringify(body) });
}

/** 创建标签；名称按规范化后唯一，因此大小写不同视为同名。 */
export function createTag(body: { name: string; description: string }) {
 return request<Tag>('/api/v1/admin/tags', { method: 'POST', body: JSON.stringify(body) });
}

/** 修改标签。 */
export function updateTag(tagId: string, body: { name: string; description: string }) {
 return request<Tag>(`/api/v1/admin/tags/${encodeURIComponent(tagId)}`, { method: 'PUT', body: JSON.stringify(body) });
}

/** 主题处置：锁定、置顶或移动；原因必填并写入审计。 */
export function moderateTopic(topicId: string, body: { reason: string; is_locked?: boolean; is_pinned?: boolean; board_id?: string }) {
 return request<PublicContent>(`/api/v1/admin/topics/${encodeURIComponent(topicId)}`, { method: 'PATCH', body: JSON.stringify(body) });
}

/** 读取删除原文：原因必填并写入审计；超过 30 天保留期返回 410。 */
export function readDeletedArchive(body: { content_id: string; reason: string }) {
 return request<AdminDeletedArchive>('/api/v1/admin/deleted-archives/read', { method: 'POST', body: JSON.stringify(body) });
}

/** 匿名追溯：返回作者与本次追溯的审计 ID；原因必填并写入审计。 */
export function traceAnonymousAuthor(body: { content_id: string; reason: string }) {
 return request<AdminAnonymousTrace>('/api/v1/admin/anonymous-traces', { method: 'POST', body: JSON.stringify(body) });
}
