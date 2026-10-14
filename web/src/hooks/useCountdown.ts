import { useEffect, useState } from 'react';

/**
 * Second-by-second countdown used to keep "发送验证码" disabled until the
 * server's retry_after_seconds has elapsed.
 */
export function useCountdown(): [number, (seconds: number) => void] {
 const [seconds, setSeconds] = useState(0);

 useEffect(() => {
  if (seconds <= 0) return undefined;
  const timer = window.setTimeout(() => setSeconds(value => (value > 0 ? value - 1 : 0)), 1000);
  return () => window.clearTimeout(timer);
 }, [seconds]);

 return [seconds, setSeconds];
}
