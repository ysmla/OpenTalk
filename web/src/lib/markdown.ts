import MarkdownIt from 'markdown-it';

/**
 * Safe Markdown rendering for stored user content.
 *
 * This file MUST stay in sync with the server validator in
 * `server/internal/markdown/markdown.go`. The server only ever accepts Markdown
 * that this renderer can display safely, so the rules here are a mirror of the
 * server's, not an independent invention:
 *
 * - raw HTML is never allowed: `html: false` makes markdown-it escape every
 *   `<`, `>` and `&` it finds, so no source HTML reaches the output as markup;
 * - the extension set is the same as the server parser's — linkify, tables and
 *   strikethrough — because a link the server never saw must not be created
 *   here. Bare URLs are therefore autolinked on both sides;
 * - the link and image protocol whitelist is the server's `allowedProtocols`
 *   (`http`, `https`, `mailto`), plus relative and fragment URLs, which carry
 *   no scheme at all. Everything else — `javascript:`, `data:`, `vbscript:`,
 *   `file:` — is refused by default, so a scheme a browser starts executing is
 *   denied without a code change here.
 *
 * Any change to the whitelist, the extension set or the HTML setting must be
 * made in both files in the same change.
 */

// The server's allowedProtocols map (server/internal/markdown/markdown.go).
const ALLOWED_PROTOCOLS = new Set(['http', 'https', 'mailto']);

/**
 * Mirror of the server's `unsafeDestination`. It lower-cases the value and
 * strips control characters first, because browsers ignore embedded tabs and
 * newlines, so `java\tscript:` is executable and a naive prefix check misses
 * it. A URL without a colon, or with a slash before the colon, carries no
 * scheme and is relative — the server treats both as safe, and so does this.
 */
export function isUnsafeDestination(destination: string): boolean {
 // Control characters are stripped, not escaped: the regex covers U+0000–U+001F and U+007F.
 const value = destination.replace(/[\u0000-\u001f\u007f]/g, '').toLowerCase();
 const colon = value.indexOf(':');
 if (colon < 0) return false;
 const slash = value.indexOf('/');
 if (slash >= 0 && slash < colon) return false;
 return !ALLOWED_PROTOCOLS.has(value.slice(0, colon));
}

const md = new MarkdownIt({
 html: false,
 // Matches the server's extension.Linkify: an autolinked bare URL is a link
 // node the whitelist below must still inspect.
 linkify: true,
 breaks: false,
 typographer: false,
 xhtmlOut: false,
});

/** `Token.attrGet` is typed as `string | number | null`; URLs are always strings. */
function attribute(token: { attrGet: (name: string) => string | number | null }, name: string): string | undefined {
 const value = token.attrGet(name);
 return typeof value === 'string' ? value : undefined;
}

// markdown-it's own default already refuses `javascript:`/`data:`, but the
// decision must come from the shared whitelist above so the two sides cannot
// drift apart. A destination the parser refuses never becomes a link token:
// the source stays in the document as escaped literal text (verified against
// markdown-it 15.0.2), so the author sees what was dropped.
md.validateLink = (url: string) => !isUnsafeDestination(url);

// Defence in depth for the rules above: in markdown-it 15 a refused link has no
// token to reach these renderers, but if a future version ever produces one,
// the token itself must not turn back into an anchor.
md.renderer.rules.link_open = (tokens, idx, options, _env, self) => {
 const token = tokens[idx];
 const href = attribute(token, 'href');
 if (href !== undefined && isUnsafeDestination(href)) {
  // `nesting` is 1, so the matching link_close is dropped as well.
  return '';
 }
 return self.renderToken(tokens, idx, options);
};
md.renderer.rules.link_close = (tokens, idx, options, _env, self) => {
 // Walk back to the matching open token: without it, only the closer is left.
 for (let i = idx - 1; i >= 0; i -= 1) {
  if (tokens[i].type === 'link_open') {
   const href = attribute(tokens[i], 'href');
   if (href !== undefined && isUnsafeDestination(href)) return '';
   break;
  }
 }
 return self.renderToken(tokens, idx, options);
};

// An image with a refused destination renders as its alt text only: no request
// is made, and nothing that could be a vector (SVG in a data URL) is shown.
md.renderer.rules.image = (tokens, idx, options, env, self) => {
 const src = attribute(tokens[idx], 'src');
 if (src !== undefined && isUnsafeDestination(src)) {
  return md.utils.escapeHtml(self.renderInlineAsText(tokens[idx].children ?? [], options, env));
 }
 return self.renderToken(tokens, idx, options);
};

/**
 * Render stored Markdown to an HTML string.
 *
 * The result is safe to pass to `dangerouslySetInnerHTML`, and the proof is
 * structural rather than a post-hoc filter:
 *
 * 1. `html: false` means markdown-it never emits an author-written tag; HTML in
 *    the source is escaped into text (`&lt;script&gt;`), not markup.
 * 2. Every attribute value markdown-it writes is escaped by `escapeHtml`, and
 *    every link or image destination is checked by `validateLink` and by the
 *    renderer rules above before it can reach an `href`/`src`.
 * 3. The only URL sinks in the document are `href` and `src`, and both are
 *    restricted to the server's whitelist, so no `javascript:` or `data:` URL
 *    can survive.
 *
 * The rendered HTML is therefore a subset of markdown-it's own safe output.
 * Even so, callers should treat this as the single rendering entry point: no
 * other component may build markup from user input.
 */
export function renderMarkdown(source: string): string {
 return md.render(source);
}

/** Render a single line (a preview of a title or a one-line comment). */
export function renderMarkdownInline(source: string): string {
 return md.renderInline(source);
}
