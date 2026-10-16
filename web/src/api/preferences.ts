import { request } from './client';
import type { components } from './generated';

export type NotificationPreferences = components['schemas']['NotificationPreferences'];
export type TopicSubscription = components['schemas']['TopicSubscription'];
export type EmailOverride = TopicSubscription['email_override'];

/**
 * `GET /api/v1/me/notification-preferences` — 全局邮件偏好，仅本人可读。
 *
 * 新账号的默认值由服务端在创建账号时写入（回答与回复默认开、评论默认关），所以这里
 * 展示的就是账号里实际存的值；只有"没有任何偏好行"的账号才读到三项全 false。
 */
export function getNotificationPreferences(signal?: AbortSignal) {
 return request<NotificationPreferences>('/api/v1/me/notification-preferences', { signal });
}

/**
 * `PUT /api/v1/me/notification-preferences` — 整体替换三项。
 *
 * 合同要求三项齐全（缺项按 422 拒绝），所以调用方必须提交完整对象：把部分
 * 更新当成"其余关掉"是这里最容易犯的错，接口形状就是为了防它。
 */
export function setNotificationPreferences(body: NotificationPreferences) {
 return request<NotificationPreferences>('/api/v1/me/notification-preferences', {
  method: 'PUT',
  body: JSON.stringify(body),
 });
}

/** `GET /api/v1/me/subscriptions/{topic_id}` — 没有设置行时读作 inherit。 */
export function getSubscription(topicId: string, signal?: AbortSignal) {
 return request<TopicSubscription>(`/api/v1/me/subscriptions/${encodeURIComponent(topicId)}`, { signal });
}

/**
 * `PUT /api/v1/me/subscriptions/{topic_id}` — 单篇覆盖全局邮件设置。
 *
 * 请求体里的 topic_id 必须与路径一致（服务端按 422 拒绝不一致的请求），
 * 所以这里由同一个参数生成两处，调用方没有机会让它们不同。
 */
export function setSubscription(topicId: string, emailOverride: EmailOverride) {
 const body: TopicSubscription = { topic_id: topicId, email_override: emailOverride };
 return request<TopicSubscription>(`/api/v1/me/subscriptions/${encodeURIComponent(topicId)}`, {
  method: 'PUT',
  body: JSON.stringify(body),
 });
}

/**
 * `POST /api/v1/subscriptions/unsubscribe` — 凭邮件里的签名令牌取消该主题的订阅。
 *
 * 不需要登录：令牌本身只命名一个账号与一个主题，最坏情况只是取消那一篇的订阅。服务端
 * 成功后删除该主题的设置行，因此该主题回到"跟随全局邮件偏好"。这里的调用发生在用户点击
 * 确认之后，GET 打开退订页不会走到这里，因此邮件扫描器跟随链接不会误退订。
 */
export function unsubscribeByToken(token: string) {
 return request<void>('/api/v1/subscriptions/unsubscribe', {
  method: 'POST',
  body: JSON.stringify({ token }),
 });
}
