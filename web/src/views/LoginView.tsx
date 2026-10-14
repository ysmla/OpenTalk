import { useState, type FormEvent } from 'react';
import { getCaptcha, login, type CaptchaChallenge, type CurrentUser } from '../api/auth';
import { setCSRFToken } from '../api/client';
import { Alert, Status } from '../components/Alert';
import { CaptchaField } from '../components/CaptchaField';
import { Field } from '../components/Field';
import { describeError, errorCode, type Failure } from '../lib/messages';
import { captchaAnswerProblem, emailProblem } from '../lib/validate';

type LoginViewProps = {
 /** Hand-off message from another view (注册成功 / 密码已重置). */
 notice?: string;
 initialEmail?: string;
 onAuthenticated: (user: CurrentUser) => void;
};

export function LoginView({ notice, initialEmail = '', onAuthenticated }: LoginViewProps) {
 const [email, setEmail] = useState(initialEmail);
 const [password, setPassword] = useState('');
 // The captcha block stays hidden until the server asks for it, then stays
 // visible for the retry.
 const [captchaRequired, setCaptchaRequired] = useState(false);
 const [captcha, setCaptcha] = useState<CaptchaChallenge>();
 const [captchaAnswer, setCaptchaAnswer] = useState('');
 const [captchaPending, setCaptchaPending] = useState(false);
 const [submitting, setSubmitting] = useState(false);
 const [failure, setFailure] = useState<Failure>();

 async function loadCaptcha(reportFailure: boolean) {
  setCaptchaPending(true);
  setCaptchaAnswer('');
  try {
   setCaptcha(await getCaptcha('login'));
  } catch (error) {
   setCaptcha(undefined);
   if (reportFailure) setFailure(describeError(error));
  } finally {
   setCaptchaPending(false);
  }
 }

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (submitting) return;
  const problem = emailProblem(email)
   ?? (password ? undefined : '请填写密码。')
   ?? (captchaRequired ? captchaAnswerProblem(captchaAnswer) : undefined)
   ?? (captchaRequired && !captcha ? '图形验证码尚未加载完成，请点击“换一张”后重试。' : undefined);
  if (problem) {
   setFailure({ text: problem });
   return;
  }
  setSubmitting(true);
  setFailure(undefined);
  try {
   const session = await login({
    email: email.trim(),
    password,
    ...(captchaRequired && captcha
     ? { captcha_challenge_id: captcha.challenge_id, captcha_answer: captchaAnswer.trim() }
     : {}),
   });
   setCSRFToken(session.csrf_token);
   setPassword('');
   onAuthenticated(session.user);
  } catch (error) {
   setFailure(describeError(error));
   if (errorCode(error) === 'CAPTCHA_REQUIRED') {
    setCaptchaRequired(true);
    await loadCaptcha(false);
   }
  } finally {
   setSubmitting(false);
  }
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>登录</h1>
  <p>登录后可以查看和编辑个人资料。</p>
  <Status>{notice}</Status>
  <form onSubmit={handleSubmit} noValidate>
   <Field label="邮箱" type="email" autoComplete="email" value={email} onChange={setEmail} disabled={submitting} />
   <Field label="密码" type="password" autoComplete="current-password" value={password} onChange={setPassword} disabled={submitting} />
   {captchaRequired && <CaptchaField
    challenge={captcha}
    answer={captchaAnswer}
    onAnswerChange={setCaptchaAnswer}
    onRefresh={() => { void loadCaptcha(true); }}
    refreshing={captchaPending}
   />}
   <Alert failure={failure} />
   <div className="actions">
    <button type="submit" disabled={submitting}>{submitting ? '正在登录…' : '登录'}</button>
    <a href="#/register">注册新账号</a>
    <a href="#/reset">忘记密码？</a>
   </div>
  </form>
  <nav className="links" aria-label="返回">
   <a href="#/">返回首页</a>
  </nav>
 </main>;
}
