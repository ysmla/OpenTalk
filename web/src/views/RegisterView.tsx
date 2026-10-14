import { useState, type FormEvent } from 'react';
import { register } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { CaptchaField } from '../components/CaptchaField';
import { Field } from '../components/Field';
import { useEmailCodeFlow } from '../hooks/useEmailCodeFlow';
import { describeError, errorCode } from '../lib/messages';
import { charCount, emailCodeProblem, emailProblem, nicknameProblem, passwordProblem, NICKNAME_MAX } from '../lib/validate';

type RegisterViewProps = {
 onRegistered: (email: string) => void;
};

export function RegisterView({ onRegistered }: RegisterViewProps) {
 const [email, setEmail] = useState('');
 const [emailCode, setEmailCode] = useState('');
 const [password, setPassword] = useState('');
 const [nickname, setNickname] = useState('');
 const [submitting, setSubmitting] = useState(false);
 const flow = useEmailCodeFlow('register');
 const { captcha, captchaAnswer, setCaptchaAnswer, refreshCaptcha, captchaPending, challenge, seconds, sending, send, failure, setFailure, notice } = flow;

 async function handleSendCode() {
  if (sending || seconds > 0) return;
  const problem = emailProblem(email);
  if (problem) {
   setFailure({ text: problem });
   return;
  }
  await send(email.trim());
 }

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (submitting) return;
  const problem = emailProblem(email)
   ?? emailCodeProblem(emailCode)
   ?? passwordProblem(password)
   ?? nicknameProblem(nickname)
   ?? (challenge ? undefined : '请先获取邮件验证码。');
  if (problem) {
   setFailure({ text: problem });
   return;
  }
  if (!challenge) return;
  setSubmitting(true);
  setFailure(undefined);
  try {
   await register({
    email: email.trim(),
    challenge_id: challenge.challenge_id,
    email_code: emailCode.trim(),
    password,
    nickname,
   });
   setPassword('');
   onRegistered(email.trim());
  } catch (error) {
   setFailure(describeError(error));
   if (errorCode(error) === 'EMAIL_CODE_INVALID') setEmailCode('');
  } finally {
   setSubmitting(false);
  }
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>注册账号</h1>
  <p>使用邮箱注册。注册成功后不会自动登录，需要用新账号登录一次。</p>
  <form onSubmit={handleSubmit} noValidate>
   <Field label="邮箱" type="email" autoComplete="email" value={email} onChange={setEmail} disabled={submitting} />
   <CaptchaField
    challenge={captcha}
    answer={captchaAnswer}
    onAnswerChange={setCaptchaAnswer}
    onRefresh={refreshCaptcha}
    refreshing={captchaPending}
   />
   <div className="actions">
    <button type="button" className="secondary" onClick={handleSendCode} disabled={sending || seconds > 0}>
     {sending ? '正在发送…' : seconds > 0 ? `${seconds} 秒后可重新发送` : '发送验证码'}
    </button>
    {seconds > 0 && <span className="hint" role="status">已发送，请查收邮件。</span>}
   </div>
   <Field label="邮件验证码" autoComplete="one-time-code" value={emailCode} onChange={setEmailCode} hint="请填写邮件里收到的验证码。" disabled={submitting} />
   <Field label="密码" type="password" autoComplete="new-password" value={password} onChange={setPassword} hint="8–16 个字符，至少包含一个字母和一个数字。" disabled={submitting} />
   <Field
    label="昵称"
    autoComplete="nickname"
    value={nickname}
    onChange={setNickname}
    hint={`最多 ${NICKNAME_MAX} 个字符，仅限汉字或字母（当前 ${charCount(nickname)}）。`}
    disabled={submitting}
   />
   <Status>{notice}</Status>
   <Alert failure={failure} />
   <div className="actions">
    <button type="submit" disabled={submitting}>{submitting ? '正在注册…' : '注册'}</button>
    <a href="#/login">已有账号，去登录</a>
   </div>
  </form>
  <nav className="links" aria-label="返回">
   <a href="#/">返回首页</a>
  </nav>
 </main>;
}
