import { renderMarkdown } from '../lib/markdown';

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
 */
export function Markdown({ source }: { source: string }) {
 return <div className="markdown" dangerouslySetInnerHTML={{ __html: renderMarkdown(source) }} />;
}
