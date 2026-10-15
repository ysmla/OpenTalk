import type { components } from './generated';

export class APIError extends Error {
 constructor(
  public readonly status: number,
  public readonly code: string,
  // Server-supplied display text. `message` stays the machine code so existing
  // callers that read `error.message` keep the previous behaviour.
  public readonly serverMessage?: string,
  // Seconds from the Retry-After response header, when the API sends one.
  public readonly retryAfterSeconds?: number,
 ) {
  super(code);
 }
}

function readRetryAfter(response: Response): number | undefined {
 const seconds = Number(response.headers.get('Retry-After'));
 return Number.isFinite(seconds) && seconds > 0 ? Math.ceil(seconds) : undefined;
}

let csrfToken: string | undefined;
export function setCSRFToken(token?: string) { csrfToken = token; }

/** The in-memory write token; only an upload that bypasses `request` needs it. */
export function getCSRFToken(): string | undefined { return csrfToken; }

type UnauthenticatedListener = () => void;
let onUnauthenticated: UnauthenticatedListener | undefined;

/**
 * App registers its session reset here so that a 401 from any call path —
 * including the XMLHttpRequest upload, which cannot go through `request` —
 * still drops the stale session instead of leaving the UI signed in.
 */
export function setUnauthenticatedHandler(listener?: UnauthenticatedListener) {
 onUnauthenticated = listener;
}

/** Notify App that the session is gone. Called by every 401 path in this module. */
export function notifyUnauthenticated() {
 // A 401 only means a *lost session* when this page believed it had one. A guest's
 // ordinary "not signed in" answer — the bootstrap GET /me, a login rejected for
 // wrong credentials, a public read that needs no session — must neither bounce the
 // visitor to the login screen nor replace the server's own error text with
 // "请先登录". Bouncing every 401 did exactly that: it made the public topic list
 // unreachable for logged-out visitors, because the page's own bootstrap 401 was
 // read as an expired session.
 //
 // The in-memory CSRF token is precisely that belief: it is set once a sign-in has
 // succeeded and cleared on the way out. Gating here rather than at each call site
 // is what makes the upload path inherit the same rule.
 if (csrfToken === undefined) return;
 csrfToken = undefined;
 onUnauthenticated?.();
}

/**
 * Map a failed response onto the stable machine codes the API uses, so a caller
 * that did not go through `request` can still build the same APIError.
 */
function fallbackCode(status: number): string {
 if (status === 401) return 'UNAUTHENTICATED';
 if (status === 403) return 'FORBIDDEN';
 if (status === 404) return 'NOT_FOUND';
 if (status === 413) return 'PAYLOAD_TOO_LARGE';
 if (status === 429) return 'RATE_LIMITED';
 if (status >= 500) return 'DEPENDENCY_UNAVAILABLE';
 return 'REQUEST_FAILED';
}

/** Build the same APIError `request` would have, from a status and an error body. */
export function apiErrorFrom(status: number, body: Partial<components['schemas']['ApiError']>, retryAfterSeconds?: number): APIError {
 return new APIError(status, body.code || fallbackCode(status), body.message, retryAfterSeconds);
}

// Same-origin only: never forward credentials to a caller-supplied external URL.
export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
 const base = import.meta.env.VITE_API_BASE || '';
 const destination = new URL(`${base}${path}`, window.location.origin);
 if (destination.origin !== window.location.origin) throw new Error('API must be same-origin');
 const headers = new Headers(options.headers);
 if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type', 'application/json');
 const method = (options.method || 'GET').toUpperCase();
 if (!['GET', 'HEAD', 'OPTIONS'].includes(method) && csrfToken) headers.set('X-CSRF-Token', csrfToken);
 const response = await fetch(destination, { ...options, method, headers, credentials: 'same-origin' });
 if (!response.ok) {
  const body = await response.json().catch(() => ({})) as Partial<components['schemas']['ApiError']>;
  if (response.status === 401) notifyUnauthenticated();
  throw apiErrorFrom(response.status, body, readRetryAfter(response));
 }
 if (response.status === 204) return undefined as T;
 return response.json() as Promise<T>;
}

export function getHealth(signal?: AbortSignal) {
 return request<components['schemas']['HealthStatus']>('/health', { signal });
}
