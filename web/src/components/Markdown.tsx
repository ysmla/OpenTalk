import { useEffect, useRef } from 'react';
import { renderMarkdown } from '../lib/markdown';
import { guardMarkdownAnchors } from './LinkConfirmDialog';

/**
 * The single place user Markdown becomes DOM.
 *
 * `renderMarkdown` is the safe entry point documented in
 * `web/src/lib/markdown.ts`: it mirrors the server whitelist in
 * `server/internal/markdown/markdown.go` (raw HTML disabled, links and images
 * restricted to http/https/mailto plus relative URLs). Because raw HTML is
 * escaped by markdown-it and every URL sink is protocol-checked before it can
 * be written, the string here cannot contain author-supplied markup or an
 * executable URL — see that file for the full argument. No other component may
 * build markup from user input.
 *
 * The rendered body is also where 外链 become a deliberate click: an outbound
 * link opens the 中间页 in `LinkConfirmDialog` instead of leaving the site
 * immediately (requirements.md:76). The interception lives in that module, so
 * every Markdown body — topic, answer, comment and preview — behaves the same.
 */
export function Markdown({ source }: { source: string }) {
 const host = useRef<HTMLDivElement>(null);
 // Anchors are part of the injected HTML, so they are not React children and
 // cannot carry an onClick of their own. The effect runs after every render of
 // this body, which is the point: new content gets its guard too.
 useEffect(() => {
  if (host.current !== null) guardMarkdownAnchors(host.current);
 });
 return <div className="markdown" ref={host} dangerouslySetInnerHTML={{ __html: renderMarkdown(source) }} />;
}
