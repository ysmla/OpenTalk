import type { components } from '../api/generated';
import { unreferencedAttachments } from '../lib/attachments';
import { formatBytes } from '../lib/format';

export type Attachment = components['schemas']['Attachment'];

/**
 * Images associated with a piece of content. `url` is the controlled
 * `/media/{key}` address the API returns, so it is used exactly as given: the
 * client never builds a media URL of its own.
 *
 * `bodyMd` is the rendered body: an image it already embeds is left out of the
 * strip, so one upload is not shown twice. Pass an empty string when there is no
 * body to compare against, and every attachment is listed.
 */
export function AttachmentGallery({ attachments, bodyMd = '' }: { attachments: Attachment[]; bodyMd?: string }) {
 const rest = unreferencedAttachments(bodyMd, attachments);
 if (rest.length === 0) return null;
 return <ul className="attachments">
  {rest.map(attachment => <li key={attachment.id}>
   <img src={attachment.url} alt="内容附带的图片" loading="lazy" />
   <span className="hint">{formatBytes(attachment.byte_size) ?? attachment.mime_type}</span>
  </li>)}
 </ul>;
}
