import type { ReactNode } from 'react';
import type { Failure } from '../lib/messages';

/** Error text: announced immediately. */
export function Alert({ failure }: { failure?: Failure }) {
 if (!failure) return null;
 return <p className="alert" role="alert">
  {failure.text}
  {failure.detail && <span className="detail">{failure.detail}</span>}
 </p>;
}

/** Neutral progress/success text: announced politely. */
export function Status({ children }: { children?: ReactNode }) {
 if (!children) return null;
 return <p className="status" role="status">{children}</p>;
}
