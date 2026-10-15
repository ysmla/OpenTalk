import { useCallback, useEffect, useState } from 'react';

/**
 * Every screen the hash router knows about. The parameterised screens keep
 * their argument in a typed field instead of the URL string, so a view never
 * has to parse the hash itself.
 */
export type RouteLocation =
 | { name: 'home' }
 | { name: 'login' }
 | { name: 'register' }
 | { name: 'reset' }
 | { name: 'me' }
 | { name: 'admin' }
 | { name: 'topics' }
 | { name: 'board'; id: string }
 | { name: 'tag'; id: string }
 | { name: 'topic'; id: string }
 | { name: 'new'; boardId?: string }
 | { name: 'edit'; id: string };

export type RouteName = RouteLocation['name'];

/** The nine legacy names plus the topic list, all of which carry no argument. */
export type Route = 'home' | 'login' | 'register' | 'reset' | 'me' | 'admin' | 'topics';

const STATIC_ROUTES: Record<string, Route> = {
 login: 'login',
 register: 'register',
 reset: 'reset',
 me: 'me',
 home: 'home',
 admin: 'admin',
 // Aliases for the home screen's topic list.
 topics: 'topics',
 'topics/list': 'topics',
 // The admin settings screen is reachable under either spelling.
 'admin/settings': 'admin',
};

/** Route names that are only meaningful with an id appended. */
const PARAMETERISED_ROUTES: Record<string, RouteLocation['name']> = {
 board: 'board',
 tag: 'tag',
 topic: 'topic',
 edit: 'edit',
};

/** Split `#/topic/123?x=1` into its path segments and its query string. */
function splitHash(hash: string): { segments: string[]; query: URLSearchParams } {
 const withoutHash = hash.replace(/^#\/?/, '');
 const queryAt = withoutHash.indexOf('?');
 const path = queryAt === -1 ? withoutHash : withoutHash.slice(0, queryAt);
 const query = queryAt === -1 ? '' : withoutHash.slice(queryAt + 1);
 const segments = path
  .replace(/\/+$/, '')
  .split('/')
  .filter(segment => segment !== '')
  .map(segment => segment.toLowerCase());
 return { segments, query: new URLSearchParams(query) };
}

/**
 * Parse a location hash into a route. Pure and independently callable: it reads
 * only its argument, so it stays testable and safe to call outside React.
 *
 * `#/topic/123` → topic 123, `#/new?board_id=1` → the new-topic screen with a
 * preselected board, `#/admin/settings` → admin; anything unknown (including an
 * empty or malformed hash) → home.
 */
export function parseHash(hash: string): RouteLocation {
 // A leading `#` that is not followed by `/` is a legacy in-page fragment, not a route.
 if (hash !== '' && !hash.startsWith('#')) return { name: 'home' };
 const { segments, query } = splitHash(hash);
 if (segments.length === 0) return { name: 'home' };
 const path = segments.join('/');
 const staticName = STATIC_ROUTES[path];
 if (staticName) return { name: staticName };
 // `#/new` may or may not carry a board; the query string is the only argument.
 if (segments[0] === 'new') {
  // Accept both `#/new?board_id=1` and the friendlier `#/new/1`.
  const boardId = (segments[1] ?? query.get('board_id') ?? '').trim();
  return boardId ? { name: 'new', boardId } : { name: 'new' };
 }
 const name = PARAMETERISED_ROUTES[segments[0]];
 const id = segments[1];
 if (name && id) {
  switch (name) {
   case 'board': return { name: 'board', id };
   case 'tag': return { name: 'tag', id };
   case 'topic': return { name: 'topic', id };
   case 'edit': return { name: 'edit', id };
  }
 }
 // The `Route` union predates parameterised routes; an unknown or incomplete
 // path still falls back to home, which is the behaviour every caller expects.
 return { name: 'home' };
}

/**
 * `#/login` → 'login', `#/admin/settings` → 'admin'; anything unknown
 * (including an empty hash) → 'home'. Parameterised routes ('topic', 'new',
 * 'edit', 'board', 'tag') are named but carry no id, so callers that need the
 * argument use `parseHash` instead. Never put an identity or a token in the URL.
 */
export function readRoute(hash: string): RouteName {
 const location = parseHash(hash);
 return location.name;
}

/** The canonical `#/…` fragment for a route. */
export function routeHref(location: RouteLocation): string {
 switch (location.name) {
  case 'home': return '#/';
  case 'topics': return '#/topics';
  case 'new':
   return location.boardId ? `#/new?board_id=${encodeURIComponent(location.boardId)}` : '#/new';
  case 'board': return `#/board/${encodeURIComponent(location.id)}`;
  case 'tag': return `#/tag/${encodeURIComponent(location.id)}`;
  case 'topic': return `#/topic/${encodeURIComponent(location.id)}`;
  case 'edit': return `#/edit/${encodeURIComponent(location.id)}`;
  default: return `#/${location.name}`;
 }
}

/** Set `window.location.hash` without leaving a duplicate history entry. */
export function navigateTo(location: RouteLocation) {
 const target = routeHref(location);
 if (window.location.hash === target) return;
 window.location.hash = target;
}

/**
 * Minimal hash router: no dependency, and a refresh keeps the current screen
 * because the route lives in window.location.hash.
 */
export function useHashRoute(): [RouteLocation, (location: RouteLocation) => void] {
 const [location, setLocation] = useState<RouteLocation>(() => parseHash(window.location.hash));

 useEffect(() => {
  const sync = () => setLocation(parseHash(window.location.hash));
  window.addEventListener('hashchange', sync);
  return () => window.removeEventListener('hashchange', sync);
 }, []);

 const navigate = useCallback((next: RouteLocation) => {
  const target = routeHref(next);
  if (window.location.hash === target) {
   // Same fragment: hashchange will not fire, so update the state ourselves.
   setLocation(parseHash(target));
   return;
  }
  window.location.hash = target;
 }, []);

 return [location, navigate];
}
