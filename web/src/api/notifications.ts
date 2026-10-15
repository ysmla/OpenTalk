import { request } from './client';
import type { components } from './generated';

export type Notification = components['schemas']['Notification'];
export type NotificationPage = components['schemas']['NotificationPage'];

/** `GET /api/v1/me/notifications` — 仅接收者本人可读，按最新在前。 */
export function listNotifications(query: { cursor?: string } = {}, signal?: AbortSignal) {
 const search = new URLSearchParams();
 if (query.cursor !== undefined && query.cursor !== '') search.set('cursor', query.cursor);
 const suffix = search.toString() === '' ? '' : `?${search.toString()}`;
 return request<NotificationPage>(`/api/v1/me/notifications${suffix}`, { signal });
}

/** `PUT /api/v1/me/notifications/{notification_id}/read` — 标记已读，重复标记是幂等的。 */
export function markNotificationRead(notificationId: string) {
 return request<void>(`/api/v1/me/notifications/${encodeURIComponent(notificationId)}/read`, { method: 'PUT' });
}
