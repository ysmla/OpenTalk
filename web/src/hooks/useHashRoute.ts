import { useCallback, useEffect, useState } from 'react';

export type Route = 'home' | 'login' | 'register' | 'reset' | 'me' | 'admin';

const HASH_ROUTES: Record<string, Route> = {
 login: 'login',
 register: 'register',
 reset: 'reset',
 me: 'me',
 home: 'home',
 admin: 'admin',
 // The admin settings screen is reachable under either spelling.
 'admin/settings': 'admin',
};

/**
 * `#/login` → 'login', `#/admin/settings` → 'admin'; anything unknown
 * (including an empty hash) → 'home'.
 */
export function readRoute(hash: string): Route {
 const path = hash.replace(/^#\/?/, '').split(/[?&]/)[0].replace(/\/+$/, '').toLowerCase();
 return HASH_ROUTES[path] ?? 'home';
}

/**
 * Minimal hash router: no dependency, and a refresh keeps the current screen
 * because the route lives in window.location.hash.
 */
export function useHashRoute(): [Route, (route: Route) => void] {
 const [route, setRoute] = useState<Route>(() => readRoute(window.location.hash));

 useEffect(() => {
  const sync = () => setRoute(readRoute(window.location.hash));
  window.addEventListener('hashchange', sync);
  return () => window.removeEventListener('hashchange', sync);
 }, []);

 const navigate = useCallback((next: Route) => {
  const target = `#/${next}`;
  if (window.location.hash === target) {
   setRoute(next);
   return;
  }
  window.location.hash = target;
 }, []);

 return [route, navigate];
}
