import type { components } from '../api/generated';
import { formatBytes } from '../lib/format';

export type Attachment = components['schemas']['Attachment'];

/**
 * Images associated with a piece of content. `url` is the controlled
 * `/media/{key}` address the API returns, so it is used exactly as given: the
 * client never builds a media URL of its own.
 */
export function AttachmentGallery({ attachments }: { attachments: Attachment[] }) {
 if (attachments.length === 0) return null;
 return <ul className="attachments">
  {attachments.map(attachment => <li key={attachment.id}>
   <img src={attachment.url} alt="内容附带的图片" loading="lazy" />
   <span className="hint">{formatBytes(attachment.byte_size) ?? attachment.mime_type}</span>
  </li>)}
 </ul>;
}
