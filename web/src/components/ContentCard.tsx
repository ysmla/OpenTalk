import { isDeleted, isQuestion, type PublicContent } from '../api/content';
import { authorLabel, TagList, TopicStatus } from './ContentMeta';
import { Markdown } from './Markdown';
import { formatDateTime } from '../lib/format';

/**
 * 一条内容在列表里的通用渲染：本人内容页、公开主页与搜索结果都用它。
 *
 * 混排列表（帖子、回答、评论）的共同点是：只有帖子有自己的页面，回答与评论要回到所属主题。删除占位
 * 只显示"已删除"与服务端给的文案——服务端不会下发原文与作者，界面也不该猜。
 */
export function ContentCard({ item }: { item: PublicContent }) {
 if (isDeleted(item)) {
  return <article className="result">
   <p className="byline"><span className="chip">已删除</span></p>
   <p className="hint">{item.message}</p>
  </article>;
 }
 const isTopic = item.kind === 'discussion' || item.kind === 'question';
 const href = isTopic ? `#/topic/${encodeURIComponent(item.id)}` : `#/topic/${encodeURIComponent(item.topic_id)}`;
 const createdAt = formatDateTime(item.created_at);
 return <article className="result">
  <p className="byline">
   <span className="chip">{item.kind === 'question' ? '问题' : item.kind === 'answer' ? '回答' : item.kind === 'comment' ? '评论' : '帖子'}</span>
   {authorLabel(item.author)}
   {createdAt && <span className="time">{createdAt}</span>}
  </p>
  {isTopic && <h2><a href={href}>{item.title}</a></h2>}
  {!isTopic && <p className="hint">在 <a href={href}>该主题</a> 下</p>}
  <Markdown source={item.body_md} />
  {isTopic && item.tags.length > 0 && <TagList tags={item.tags} />}
  {isTopic && isQuestion(item) && <TopicStatus topic={item} />}
 </article>;
}
