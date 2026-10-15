import { apiErrorFrom, getCSRFToken, notifyUnauthenticated, type APIError } from './client';
import type { components } from './generated';

export type Attachment = components['schemas']['Attachment'];

/**
 * `POST /api/v1/attachments` — multipart upload of one image.
 *
 * This is the one call that cannot use `request`: the contract wants upload
 * progress, and `fetch` still cannot report request progress. It therefore
 * repeats `request`'s rules by hand — same-origin only, `credentials:
 * 'same-origin'`, the in-memory CSRF token on the write, no CSRF handling left
 * to the caller, and the same APIError shape (including the 401 session reset).
 */
export function uploadImage(file: File, onProgress?: (percent: number) => void): Promise<Attachment> {
 return new Promise<Attachment>((resolve, reject) => {
  const base = import.meta.env.VITE_API_BASE || '';
  const destination = new URL(`${base}/api/v1/attachments`, window.location.origin);
  if (destination.origin !== window.location.origin) {
   reject(new Error('API must be same-origin'));
   return;
  }
  const form = new FormData();
  form.append('file', file);
  const xhr = new XMLHttpRequest();
  xhr.open('POST', destination);
  xhr.withCredentials = true;
  const csrf = getCSRFToken();
  if (csrf) xhr.setRequestHeader('X-CSRF-Token', csrf);
  xhr.upload.onprogress = event => {
   if (event.lengthComputable && onProgress) onProgress(Math.round((event.loaded / event.total) * 100));
  };
  xhr.onerror = () => reject(apiErrorFrom(0, { code: 'NETWORK_ERROR', message: '网络异常，图片上传失败。' }));
  xhr.ontimeout = () => reject(apiErrorFrom(0, { code: 'NETWORK_ERROR', message: '图片上传超时。' }));
  xhr.onload = () => {
   let body: Partial<components['schemas']['ApiError']> = {};
   try {
    body = JSON.parse(xhr.responseText) as Partial<components['schemas']['ApiError']>;
   } catch {
    // A non-JSON body means the server failed before it could answer properly.
   }
   if (xhr.status >= 200 && xhr.status < 300) {
    try {
     resolve(JSON.parse(xhr.responseText) as Attachment);
    } catch {
     reject(apiErrorFrom(xhr.status, { code: 'INVALID_RESPONSE', message: '图片上传成功，但服务端返回无法解析。' }));
    }
    return;
   }
   if (xhr.status === 401) notifyUnauthenticated();
   const retryAfter = Number(xhr.getResponseHeader('Retry-After'));
   reject(apiErrorFrom(xhr.status, body, Number.isFinite(retryAfter) && retryAfter > 0 ? Math.ceil(retryAfter) : undefined));
  };
  xhr.send(form);
 });
}

/** Exported for callers that keep an upload error around as `Failure`. */
export type UploadFailure = APIError;
