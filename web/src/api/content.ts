import { request } from './client';
import type { components } from './generated';

export type PublicContent = components['schemas']['PublicContent'];
export type DiscussionTopic = components['schemas']['DiscussionTopic'];
export type QuestionTopic = components['schemas']['QuestionTopic'];
export type Answer = components['schemas']['Answer'];
export type Comment = components['schemas']['Comment'];
export type DeletedContent = components['schemas']['DeletedContent'];
export type Topic = DiscussionTopic | QuestionTopic;
export type Board = components['schemas']['Board'];
export type Tag = components['schemas']['Tag'];
export type Attachment = components['schemas']['Attachment'];
export type ContentPage = components['schemas']['ContentPage'];
export type ContentPermissions = components['schemas']['ContentPermissions'];
export type CreateTopicRequest = components['schemas']['CreateTopicRequest'];
export type CreateAnswerRequest = components['schemas']['CreateAnswerRequest'];
export type CreateCommentRequest = components['schemas']['CreateCommentRequest'];
export type EditContentRequest = components['schemas']['EditContentRequest'];
export type SetResolutionRequest = components['schemas']['SetResolutionRequest'];
export type DeletionResult = components['schemas']['DeletionResult'];

export type TopicSort = 'newest' | 'active';
export type ResolutionStatus = SetResolutionRequest['resolution_status'];

/**
 * `GET /api/v1/boards` — 板块决定发帖是普通帖还是提问。
 * Paged by the contract; callers that need the whole catalogue use
 * `loadAllPages` (or `listAllBoards`) rather than reading one page.
 */
export function listBoards(query: { cursor?: string } = {}, signal?: AbortSignal) {
 const params = queryString({ cursor: query.cursor });
 return request<components['schemas']['BoardPage']>(`/api/v1/boards${params}`, { signal });
}

/** Every board, following `next_cursor` to the end. */
export function listAllBoards(signal?: AbortSignal) {
 return loadAllPages((cursor, pageSignal) => listBoards({ cursor }, pageSignal ?? signal), signal);
}

/** `GET /api/v1/tags` — 第一版由管理员维护，发帖时选择。 */
export function listTags(query: { cursor?: string; q?: string } = {}, signal?: AbortSignal) {
 const params = queryString({ cursor: query.cursor, q: query.q });
 return request<components['schemas']['TagPage']>(`/api/v1/tags${params}`, { signal });
}

/** Every tag the server offers, following `next_cursor` to the end. */
export function listAllTags(signal?: AbortSignal) {
 return loadAllPages((cursor, pageSignal) => listTags({ cursor }, pageSignal ?? signal), signal);
}

type TopicQuery = {
 cursor?: string;
 board_id?: string;
 tag_id?: string;
 resolution_status?: ResolutionStatus;
 sort?: TopicSort;
};

/** Search parameters. `q` is required by the contract; the server refuses an empty one. */
export type SearchQuery = {
 q: string;
 cursor?: string;
 board_id?: string;
 tag_id?: string;
};

/** Query strings skip unset values; an empty board_id must not become `board_id=`. */function queryString(params: Record<string, string | undefined>): string {
 const search = new URLSearchParams();
 for (const [key, value] of Object.entries(params)) {
  if (value !== undefined && value !== '') search.set(key, value);
 }
 const query = search.toString();
 return query === '' ? '' : `?${query}`;
}

/** `GET /api/v1/topics` — 公开列表，过滤隐藏内容与匿名身份。 */
export function listTopics(query: TopicQuery, signal?: AbortSignal) {
 const params = queryString({
  cursor: query.cursor,
  board_id: query.board_id,
  tag_id: query.tag_id,
  resolution_status: query.resolution_status,
  sort: query.sort,
 });
 return request<ContentPage>(`/api/v1/topics${params}`, { signal });
}

/** `GET /api/v1/search` — 公开搜索；q 必填，服务端按内容 id 倒序返回。 */
export function searchContents(query: SearchQuery, signal?: AbortSignal) {
 const params = queryString({
  q: query.q,
  cursor: query.cursor,
  board_id: query.board_id,
  tag_id: query.tag_id,
 });
 return request<ContentPage>(`/api/v1/search${params}`, { signal });
}

/** `GET /api/v1/contents/{content_id}` — 主题、回答或评论，也可能是删除占位。 */
export function getContent(contentId: string, signal?: AbortSignal) {
 return request<PublicContent>(`/api/v1/contents/${encodeURIComponent(contentId)}`, { signal });
}

/** `GET /api/v1/contents/{content_id}/permissions` — 只对当前会话有效。 */
export function getContentPermissions(contentId: string, signal?: AbortSignal) {
 return request<ContentPermissions>(`/api/v1/contents/${encodeURIComponent(contentId)}/permissions`, { signal });
}

/** `GET /api/v1/questions/{topic_id}/answers` — 只对问题有意义。 */
export function listAnswers(topicId: string, cursor?: string, signal?: AbortSignal) {
 const params = queryString({ cursor });
 return request<ContentPage>(`/api/v1/questions/${encodeURIComponent(topicId)}/answers${params}`, { signal });
}

type CommentQuery = {
 cursor?: string;
 /** 评论回答时必填；评论主题时省略。 */
 answer_id?: string;
 /** 只取某条顶层评论的回复时填写。 */
 parent_comment_id?: string;
};

/** `GET /api/v1/topics/{topic_id}/comments` — 省略 answer_id 表示主题评论。 */
export function listComments(topicId: string, query: CommentQuery, signal?: AbortSignal) {
 const params = queryString({ cursor: query.cursor, answer_id: query.answer_id, parent_comment_id: query.parent_comment_id });
 return request<ContentPage>(`/api/v1/topics/${encodeURIComponent(topicId)}/comments${params}`, { signal });
}

/** `POST /api/v1/topics` — 普通帖或问题由所选板块类型决定。 */
export function createTopic(body: CreateTopicRequest) {
 return request<PublicContent>('/api/v1/topics', { method: 'POST', body: JSON.stringify(body) });
}

/** `PATCH /api/v1/contents/{content_id}` — 省略附件清单表示不改，空数组表示解除全部关联。 */
export function editContent(contentId: string, body: EditContentRequest) {
 return request<PublicContent>(`/api/v1/contents/${encodeURIComponent(contentId)}`, { method: 'PATCH', body: JSON.stringify(body) });
}

/** `DELETE /api/v1/contents/{content_id}` — 同一事务内连带删除后代。 */
export function deleteContent(contentId: string) {
 return request<DeletionResult>(`/api/v1/contents/${encodeURIComponent(contentId)}`, { method: 'DELETE' });
}

/** `POST /api/v1/answers` — 作者由登录会话推导，不能由客户端指定。 */
export function createAnswer(body: CreateAnswerRequest) {
 return request<Answer>('/api/v1/answers', { method: 'POST', body: JSON.stringify(body) });
}

/** `POST /api/v1/comments` — 顶层评论省略 reply_to_comment_id。 */
export function createComment(body: CreateCommentRequest) {
 return request<Comment>('/api/v1/comments', { method: 'POST', body: JSON.stringify(body) });
}

/** `PATCH /api/v1/questions/{topic_id}/resolution` — 仅提问者可操作。 */
export function setResolution(topicId: string, body: SetResolutionRequest) {
 return request<QuestionTopic>(`/api/v1/questions/${encodeURIComponent(topicId)}/resolution`, { method: 'PATCH', body: JSON.stringify(body) });
}

/** Narrowing helpers for the `PublicContent` union. */
export function isTopic(content: PublicContent): content is Topic {
 return content.kind === 'discussion' || content.kind === 'question';
}

export function isQuestion(content: PublicContent): content is QuestionTopic {
 return content.kind === 'question';
}

export function isDeleted(content: PublicContent): content is DeletedContent {
 return content.state === 'deleted';
}

/**
 * The discussion a piece of content belongs to.
 *
 * A topic *is* the discussion, an answer and a comment name theirs in
 * `topic_id`. This is what an editor returns to after saving: a comment has no
 * page of its own, and landing on `#/topic/{comment_id}` would show "该地址不是主题".
 * Undefined for a deletion placeholder, which carries no usable location.
 */
export function topicIdOf(content: PublicContent): string | undefined {
 if (isDeleted(content)) return undefined;
 if (isTopic(content)) return content.id;
 return content.topic_id;
}

/**
 * Follow an opaque cursor to the end of a list.
 *
 * The editor needs the whole board and tag catalogue, not just the first page:
 * `GET /api/v1/boards` and `GET /api/v1/tags` page with `next_cursor`, so a
 * screen that stopped after one page would silently hide choices. `maxPages`
 * is a safety net against a server that never reports `has_more: false`.
 */
export async function loadAllPages<T>(
 fetchPage: (cursor: string | undefined, signal?: AbortSignal) => Promise<{ items: T[]; has_more: boolean; next_cursor?: string }>,
 signal?: AbortSignal,
 maxPages = 20,
): Promise<T[]> {
 const collected: T[] = [];
 let cursor: string | undefined;
 for (let page = 0; page < maxPages; page += 1) {
  const current = await fetchPage(cursor, signal);
  if (signal?.aborted) return collected;
  collected.push(...current.items);
  if (!current.has_more || current.next_cursor === undefined) break;
  cursor = current.next_cursor;
 }
 return collected;
}
