import type { components } from '../api/generated';

export type ContentAuthor = components['schemas']['ContentAuthor'];
export type Tag = components['schemas']['Tag'];

/**
 * How an author is shown in a list, on a detail page and in a preview. The API
 * contract decides what a client may see:
 *
 * - `NamedAuthor` carries a public profile, so the nickname is shown as plain
 *   text. There is deliberately no link to a profile page: the router has no
 *   user route yet, and a link that fell back to home would be worse than none.
 * - `AnonymousAuthor` carries only `display_name` (楼主 or 匿名#N) and must not
 *   be related to a user in any way — the generated type has no `user` field in
 *   that branch, so reaching for one is a compile error, not a runtime mistake.
 */
export function authorLabel(author: ContentAuthor): React.JSX.Element {
 if (author.mode === 'anonymous') {
  // `display_name` is already 楼主 or 匿名#N: never render more than that.
  return <span className="author anonymous">{author.display_name}</span>;
 }
 return <span className="author">
  {author.user.nickname}
  {author.is_topic_owner && <span className="chip owner">楼主</span>}
 </span>;
}

/**
 * The 「回复 @xxx」 marker for a nested reply. `reply_to_comment_id` names the
 * comment actually being answered, which is not necessarily the thread's
 * top-level comment, so the marker follows that id and only falls back to a
 * locator when the target is not in the loaded page.
 */
export function replyTargetLabel(target: ContentAuthor | undefined, commentId: string): React.JSX.Element {
 return target
  ? <span className="reply-to">回复 {authorLabel(target)}</span>
  : <span className="reply-to" title={`回复评论 #${commentId}`}>回复 #{commentId}</span>;
}

/** Administrator-maintained tags; a topic carries at most five. */
export function TagList({ tags }: { tags: Tag[] }) {
 if (tags.length === 0) return null;
 // Each chip links to the tag-filtered list (#/tag/{id}), which reuses the topic list with `tag_id`.
 // The description stays in `title`: it is a hint, not a label competing with the name.
 return <ul className="tags">
  {tags.map(tag => <li key={tag.id} title={tag.description}>
   <a href={`#/tag/${encodeURIComponent(tag.id)}`}>{tag.name}</a>
  </li>)}
 </ul>;
}

/** Status chips: 已解决/待解决, 已锁定, 置顶. */
export function TopicStatus({ topic }: { topic: { kind: 'discussion' | 'question'; is_locked: boolean; is_pinned: boolean; resolution_status?: 'open' | 'solved' } }) {
 return <span className="statuses">
  {topic.is_pinned && <span className="chip">置顶</span>}
  {topic.kind === 'question' && (topic.resolution_status === 'solved'
   ? <span className="chip solved">已解决</span>
   : <span className="chip open">待解决</span>)}
  {topic.is_locked && <span className="chip locked">已锁定</span>}
 </span>;
}
