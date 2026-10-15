import type { components } from '../api/generated';

export type Attachment = components['schemas']['Attachment'];

/**
 * The attachments a body does not already show.
 *
 * A topic, answer or comment whose Markdown embeds `![](/media/<key>)` renders the
 * picture in the text flow. The strip underneath then repeats the same picture, so
 * one upload looks like two — which is what the browser walkthrough showed for both
 * a topic and a comment.
 *
 * The public payload carries no storage key, only the address the renderer will load
 * (`url`), so the check is a literal substring test against the body. That is the
 * same string the `<img>` is built from, so a picture counted as "already shown" is
 * genuinely the one on screen, while one referenced some other way (or not at all)
 * still appears exactly once in the strip.
 */
export function unreferencedAttachments(bodyMd: string, attachments: Attachment[]): Attachment[] {
 return attachments.filter(attachment => !bodyMd.includes(attachment.url));
}
