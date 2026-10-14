import { useId } from 'react';
import type { CaptchaChallenge } from '../api/auth';

type CaptchaFieldProps = {
 challenge?: CaptchaChallenge;
 answer: string;
 onAnswerChange: (value: string) => void;
 onRefresh: () => void;
 refreshing: boolean;
};

/**
 * Graphic captcha: the image comes from the challenge's own never-cached
 * image_url, so "换一张" only has to request a new challenge.
 */
export function CaptchaField({ challenge, answer, onAnswerChange, onRefresh, refreshing }: CaptchaFieldProps) {
 const inputId = useId();
 const hintId = `${inputId}-hint`;
 return <div className="captcha">
  <div className="captcha-image">
   {challenge
    ? <img src={challenge.image_url} alt="图形验证码图片，看不清可以点击“换一张”" />
    : <span className="hint">图形验证码加载中…</span>}
   <button type="button" className="secondary" onClick={onRefresh} disabled={refreshing}>
    {refreshing ? '正在更换…' : '换一张'}
   </button>
  </div>
  <div className="field">
   <label htmlFor={inputId}>图形验证码</label>
   <input
    id={inputId}
    value={answer}
    autoComplete="off"
    spellCheck={false}
    disabled={!challenge}
    aria-describedby={hintId}
    onChange={event => onAnswerChange(event.target.value)}
   />
   <p className="hint" id={hintId}>请按图片内容填写。验证码一次性有效，失败后请点击“换一张”。</p>
  </div>
 </div>;
}
