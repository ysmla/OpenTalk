import { useCallback, useEffect, useState } from 'react';
import {
 getCaptcha,
 sendEmailCode,
 type CaptchaChallenge,
 type EmailCodeChallenge,
 type EmailCodePurpose,
} from '../api/auth';
import { describeError, retryAfterSeconds, type Failure } from '../lib/messages';
import { captchaAnswerProblem } from '../lib/validate';
import { useCountdown } from './useCountdown';

/**
 * Shared state for the two email-code flows (注册 / 找回密码): the graphic
 * captcha, the email challenge, the resend countdown and the send errors.
 *
 * The graphic captcha is single use, so every send attempt — success or
 * failure — fetches a fresh one for the next attempt.
 */
export function useEmailCodeFlow(purpose: EmailCodePurpose) {
 const [captcha, setCaptcha] = useState<CaptchaChallenge>();
 const [captchaAnswer, setCaptchaAnswer] = useState('');
 const [captchaPending, setCaptchaPending] = useState(false);
 const [challenge, setChallenge] = useState<EmailCodeChallenge>();
 const [sending, setSending] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [seconds, setSeconds] = useCountdown();

 const loadCaptcha = useCallback(async (reportFailure: boolean) => {
  setCaptchaPending(true);
  setCaptchaAnswer('');
  try {
   setCaptcha(await getCaptcha(purpose));
  } catch (error) {
   setCaptcha(undefined);
   if (reportFailure) setFailure(describeError(error));
  } finally {
   setCaptchaPending(false);
  }
 }, [purpose]);

 useEffect(() => { void loadCaptcha(true); }, [loadCaptcha]);

 const refreshCaptcha = useCallback(() => { void loadCaptcha(true); }, [loadCaptcha]);

 const send = useCallback(async (email: string) => {
  if (sending) return;
  if (!captcha) {
   setFailure({ text: '图形验证码尚未加载完成，请点击“换一张”后重试。' });
   return;
  }
  const answerProblem = captchaAnswerProblem(captchaAnswer);
  if (answerProblem) {
   setFailure({ text: answerProblem });
   return;
  }
  setSending(true);
  setFailure(undefined);
  setNotice(undefined);
  try {
   const issued = await sendEmailCode({
    email,
    purpose,
    captcha_challenge_id: captcha.challenge_id,
    captcha_answer: captchaAnswer.trim(),
   });
   setChallenge(issued);
   setSeconds(issued.retry_after_seconds);
   setNotice(`邮件验证码已发送，${issued.retry_after_seconds} 秒后可重新发送。`);
   await loadCaptcha(true);
  } catch (error) {
   setFailure(describeError(error));
   const retryAfter = retryAfterSeconds(error);
   if (retryAfter) setSeconds(retryAfter);
   // Keep the send failure visible; the replaced captcha is only a retry aid.
   await loadCaptcha(false);
  } finally {
   setSending(false);
  }
 }, [captcha, captchaAnswer, purpose, loadCaptcha, sending, setSeconds]);

 return {
  captcha,
  captchaAnswer,
  setCaptchaAnswer,
  refreshCaptcha,
  captchaPending,
  challenge,
  seconds,
  sending,
  send,
  failure,
  setFailure,
  notice,
 };
}
