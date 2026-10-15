import type { components } from '../api/generated';

export type ContentAuthor = components['schemas']['ContentAuthor'];

/**
 * Mirrors the server's `EditWindow` (`server/internal/content/edit.go`).
 *
 * It decides only whether an edit entry is worth showing; every write re-checks
 * the window server-side, so a skewed clock can show or hide a button but can
 * never authorize anything. The two copies must change together.
 */
export const EDIT_WINDOW_MS = 30 * 60 * 1000;

/** The viewer's own identity, as far as the public payloads allow it to be known. */
export type Viewer = {
 /** `profile.public_id` of the signed-in account; undefined for a guest. */
 publicId?: string;
 /**
  * True when the viewer wrote the anonymous topic being read. The permissions
  * response calls this `force_anonymous_comment`.
  */
 isAnonymousTopicOwner: boolean;
};

/**
 * Whether one piece of content belongs to the viewer.
 *
 * Nothing in the public payloads says "this is yours": an anonymous author
 * carries no user object at all, and asking `GET /contents/{id}/permissions` for
 * every answer and every comment would turn one page into dozens of requests. So
 * ownership is derived from what is already on screen, in a way that has no false
 * positives — a wrong guess would put an edit button on somebody else's content:
 *
 * - named content is the viewer's when the author's public id matches;
 * - anonymous content is the viewer's only when it is labelled 楼主 *and* the
 *   viewer wrote that anonymous topic. 楼主 means "the topic author", so those two
 *   facts together pin it down;
 * - an anonymous participant's own `匿名#N` content is deliberately left
 *   unclaimed: deciding which number is the viewer's needs the server, so no entry
 *   is shown rather than a guess being made.
 */
export function isOwnContent(author: ContentAuthor, viewer: Viewer): boolean {
 if (author.mode === 'named') {
  return viewer.publicId !== undefined && author.user.public_id === viewer.publicId;
 }
 return viewer.isAnonymousTopicOwner && author.display_name === '楼主';
}

/**
 * The edit and delete entries for one content row. Publishing is the only action
 * the interface derives this way; the server remains the authority for every
 * write, so a stale page can only offer an action that is then refused.
 */
export function contentActions(
 content: { author: ContentAuthor; created_at: string },
 viewer: Viewer,
 options: { topicIsLocked: boolean },
): { canEdit: boolean; canDelete: boolean } {
 // A guest owns nothing, and cannot reach this by any other route: the anonymous
 // owner case implies a session.
 if (viewer.publicId === undefined) return { canEdit: false, canDelete: false };
 if (!isOwnContent(content.author, viewer)) return { canEdit: false, canDelete: false };
 const age = Date.now() - Date.parse(content.created_at);
 return {
  // A locked topic refuses body edits to everyone, which is the point of locking it.
  canEdit: !options.topicIsLocked && Number.isFinite(age) && age <= EDIT_WINDOW_MS,
  // Deleting stays available after the window closes: the permission table allows a
  // muted user to remove what they posted, and the window never applied to it.
  canDelete: true,
 };
}
