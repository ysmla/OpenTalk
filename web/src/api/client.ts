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
  if (response.status === 401) csrfToken = undefined;
  throw new APIError(response.status, body.code || 'REQUEST_FAILED', body.message, readRetryAfter(response));
 }
 if (response.status === 204) return undefined as T;
 return response.json() as Promise<T>;
}

export function getHealth(signal?: AbortSignal) {
 return request<components['schemas']['HealthStatus']>('/health', { signal });
}
