import { useEffect, useState } from 'react';
import { describeError, type Failure } from '../lib/messages';

export type CursorPage<T> = {
 items: T[];
 has_more: boolean;
 next_cursor?: string;
};

export type CursorListState<T> = {
 items: T[];
 /** True until the first page settles, successfully or not. */
 loading: boolean;
 /** True while a 加载更多 request is in flight. */
 loadingMore: boolean;
 failure?: Failure;
 hasMore: boolean;
 /** Retry the failed request without discarding the loaded items. */
 reload: () => void;
 loadMore: () => void;
 /** Add a freshly created item to the end of the accumulated list. */
 addItem: (item: T) => void;
 /** Restart from the first page, e.g. after a write changed the list. */
 refresh: () => void;
};

/**
 * Accumulating cursor list: the first page replaces everything, 加载更多 appends
 * through `next_cursor`, and any filter change is expressed by changing `key`,
 * which starts over from the first page.
 *
 * A failed first page (or a failed append) leaves the already-loaded items in
 * place so a reader never loses the list to a transient error.
 */
export function useCursorList<T>(key: string, fetchPage: (cursor: string | undefined, signal: AbortSignal) => Promise<CursorPage<T>>): CursorListState<T> {
 const [items, setItems] = useState<T[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(true);
 const [loadingMore, setLoadingMore] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [attempt, setAttempt] = useState(0);
 // The first page after a reload must replace, not append.
 const [generation, setGeneration] = useState(0);

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  setItems([]);
  setCursor(undefined);
  setHasMore(false);
  fetchPage(undefined, controller.signal).then(page => {
   if (controller.signal.aborted) return;
   setItems(page.items);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
  // `fetchPage` is intentionally not a dependency: callers pass a fresh closure
  // on every render, and re-fetching on that would loop forever.
 }, [key, attempt, generation]);

 async function loadMore() {
  if (loadingMore || loading || !hasMore || cursor === undefined) return;
  setLoadingMore(true);
  setFailure(undefined);
  try {
   const page = await fetchPage(cursor, new AbortController().signal);
   setItems(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setLoadingMore(false);
  }
 }

 function reload() {
  setAttempt(value => value + 1);
 }

 function addItem(item: T) {
  setItems(current => [...current, item]);
 }

 /** Restart from the first page, e.g. after a write changed the list. */
 function refresh() {
  setGeneration(value => value + 1);
 }

 return { items, loading, loadingMore, failure, hasMore, reload, loadMore, addItem, refresh };
}
