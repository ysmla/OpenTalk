import { useEffect, useRef, useState } from 'react';

/**
 * The 外链中间页 (requirements.md:76: 站内链接直达；外链经过中间页，展示真实域名和继续访问入口).
 *
 * Every rendered Markdown body — topic, answer and comment — goes through
 * `components/Markdown.tsx`, and all of them must behave the same way, so the
 * interception is attached once to the document root rather than threaded
 * through each view as a prop. A capture-free listener is enough: React renders
 * the anchor, no handler of ours writes to `window.location`, and the browser's
 * own navigation happens after the event has been dispatched.
 *
 * What this is and is not: it is a **user-facing** warning, not a security
 * boundary. A reader can still reach the destination by copying the URL out of
 * the anchor, and nothing here tries to stop that. Its job is what the contract
 * says — show the real domain and make leaving a deliberate click.
 *
 * The other half of that contract is 站内链接直达, and it is the half that is easy
 * to break: the guard must recognise an in-site link whatever shape it has
 * (fragment, path-relative, or an absolute URL on this origin) and leave it
 * alone. See `destinationOf`.
 */

type Pending = {
 /** The absolute URL the click was about to open. */
 href: string;
 /** What the reader should see as the destination: a hostname, or a mail address. */
 display: string;
};

let pending: Pending | undefined;
const listeners = new Set<() => void>();

function emit() {
 for (const listener of listeners) listener();
}

/** Read the current pending link. Stable identity for `useSyncExternalStore`. */
function readPending(): Pending | undefined {
 return pending;
}

function subscribe(listener: () => void): () => void {
 listeners.add(listener);
 return () => listeners.delete(listener);
}

/** Open the confirmation for one destination; a second click replaces the first. */
export function requestLinkConfirmation(href: string, display: string) {
 pending = { href, display };
 emit();
}

/** Put the dialog away without navigating. */
export function dismissLinkConfirmation() {
 if (pending === undefined) return;
 pending = undefined;
 emit();
}

/**
 * What the reader is shown for one destination, or undefined when the click
 * needs no confirmation.
 *
 * The test is **origin**, not shape: an absolute URL on this site is as much an
 * in-site link as `#/topics` or `/board/2` is, and 站内链接直达 applies to all of
 * them. Only a target on another origin is 外链. Checking the protocol alone
 * (which is all this did at first) sent every in-site link through the dialog —
 * a same-origin link resolved by `new URL` still has an http(s) scheme.
 *
 * `mailto:` has no origin to compare and is handed to the mail client rather
 * than navigated to, so it is left to the browser as well.
 *
 * The displayed domain is the URL's own `hostname`, not the visible link text:
 * the whole point is to show where the click actually goes.
 */
function destinationOf(href: string): Pending | undefined {
 let url: URL;
 try {
  url = new URL(href, window.location.href);
 } catch {
  // Nothing to promise a reader about a URL the browser itself cannot parse.
  return undefined;
 }
 if (url.protocol !== 'http:' && url.protocol !== 'https:') return undefined;
 if (url.origin === window.location.origin) return undefined;
 return { href: url.href, display: url.hostname };
}

/**
 * `event.target` is a node; the anchor is found by walking up, because the click
 * lands on whatever text or image is inside the link.
 */
function anchorOf(target: EventTarget | null): HTMLAnchorElement | undefined {
 if (!(target instanceof Element)) return undefined;
 const anchor = target.closest('a[href]');
 return anchor instanceof HTMLAnchorElement ? anchor : undefined;
}

/** One click handler that covers every Markdown body on the page. */
export function onDocumentClick(event: MouseEvent) {
 // A modified click means "open elsewhere" (new tab, download): the reader has
 // already decided, and a dialog would have nowhere to navigate.
 if (event.defaultPrevented || event.button !== 0) return;
 if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
 const anchor = anchorOf(event.target);
 if (anchor === undefined) return;
 const destination = destinationOf(anchor.getAttribute('href') ?? '');
 if (destination === undefined) return;
 event.preventDefault();
 requestLinkConfirmation(destination.href, destination.display);
}

/**
 * Attach the guard to every anchor inside one rendered Markdown body.
 *
 * A body re-renders on every parent render, so the marker keeps an anchor from
 * collecting a second listener; a replaced body has fresh anchors and therefore
 * fresh guards. Anchors the guard does not claim (in-site links, fragments) are
 * marked too, so they are not re-inspected on every pass.
 */
export function guardMarkdownAnchors(container: ParentNode) {
 for (const anchor of container.querySelectorAll('a[href]')) {
  if (!(anchor instanceof HTMLAnchorElement) || anchor.dataset.linkGuard === 'on') continue;
  anchor.dataset.linkGuard = 'on';
  anchor.addEventListener('click', onDocumentClick);
 }
}

/** The dialog itself: domain, 继续访问, 取消. */
export function LinkConfirmDialog({ onContinue }: { onContinue: (href: string) => void }) {
 const [pendingLink, setPendingLink] = useState<Pending | undefined>(readPending);
 const continueRef = useRef<HTMLButtonElement>(null);

 useEffect(() => subscribe(() => setPendingLink(readPending())), []);

 // Focus lands on 继续访问 so the keyboard path matches the visual one; the
 // dialog element itself is mounted only while a link is pending, so this runs
 // once per confirmation rather than on every render.
 useEffect(() => {
  if (pendingLink !== undefined) continueRef.current?.focus();
 }, [pendingLink]);

 if (pendingLink === undefined) return null;

 return <div className="modal" role="dialog" aria-modal="true" aria-labelledby="link-confirm-heading">
  <div className="modal-panel">
   <h2 id="link-confirm-heading">即将离开本站</h2>
   <p>你正要访问站外地址：</p>
   <p className="modal-target">{pendingLink.display}</p>
   <p className="hint">该地址由他人提供，本站无法保证其内容安全。</p>
   <div className="actions">
    <button type="button" ref={continueRef} onClick={() => onContinue(pendingLink.href)}>继续访问</button>
    <button type="button" className="secondary" onClick={dismissLinkConfirmation}>取消</button>
   </div>
  </div>
 </div>;
}
