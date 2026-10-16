import { useEffect, useState } from 'react';
import { getSubscription, setSubscription, type EmailOverride } from '../api/preferences';
import { Alert, Status } from '../components/Alert';
import { describeError, isStatus, type Failure } from '../lib/messages';

type TopicSubscriptionControlProps = {
 topicId: string;
 /** 会话已确认存在才由父级渲染；未登录时不显示本控件。 */
 signedIn: boolean;
};

/** 三个取值与合同 `email_override` 枚举一一对应。 */
const OPTIONS: { value: EmailOverride; label: string; hint: string }[] = [
 { value: 'inherit', label: '跟随全局邮件偏好', hint: '按个人资料里的三项设置决定。' },
 { value: 'enabled', label: '这个主题始终发邮件', hint: '即使全局关闭，这个主题也发。' },
 { value: 'disabled', label: '这个主题不发邮件', hint: '只关掉这个主题的邮件，站内通知照常。' },
];

/**
 * 单篇邮件订阅（`GET/PUT /me/subscriptions/{topic_id}`）。
 *
 * 这里的"订阅"只决定**邮件**：通知本身在内容写入时就已生成，站内列表不受影响。
 * 没有设置行时读作 inherit（跟随全局），所以三个选项是"跟随/开启/关闭"而不是一个
 * 开关——"没有这一行"和"明确关闭"在服务端是两件事：取消订阅（例如点邮件里的退订链接）
 * 会删掉这一行，该主题就回到跟随全局。
 *
 * 主题被删除或隐藏时服务端返回 404：读者无法为一个打不开的讨论改邮件设置，
 * 此时控件显示为不可用，而不是假装保存成功。
 */
export function TopicSubscriptionControl({ topicId, signedIn }: TopicSubscriptionControlProps) {
 const [value, setValue] = useState<EmailOverride>();
 const [saved, setSaved] = useState<EmailOverride>();
 const [loading, setLoading] = useState(true);
 const [saving, setSaving] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [reloadKey, setReloadKey] = useState(0);

 useEffect(() => {
  if (!signedIn) {
   setLoading(false);
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  getSubscription(topicId, controller.signal).then(current => {
   if (controller.signal.aborted) return;
   setValue(current.email_override);
   setSaved(current.email_override);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setFailure(isStatus(error, 404)
    ? { text: '该主题不可见或已删除，无法设置它的邮件通知。' }
    : describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [topicId, signedIn, reloadKey]);

 async function handleSubmit() {
  if (saving || value === undefined || value === saved) return;
  setSaving(true);
  setFailure(undefined);
  setNotice(undefined);
  try {
   const updated = await setSubscription(topicId, value);
   setValue(updated.email_override);
   setSaved(updated.email_override);
   setNotice(updated.email_override === 'disabled'
    ? '这个主题不再发送通知邮件；站内通知不受影响。'
    : '这个主题的邮件设置已保存。');
  } catch (error) {
   if (isStatus(error, 401)) {
    setFailure({ text: '登录状态已失效，请重新登录后再设置。' });
    return;
   }
   setFailure(describeError(error));
  } finally {
   setSaving(false);
  }
 }

 if (!signedIn) return null;

 return <section aria-labelledby="topic-subscription-heading">
  <h2 id="topic-subscription-heading">这个主题的邮件</h2>
  {loading && <Status>正在读取这个主题的邮件设置…</Status>}
  <Alert failure={failure} />
  {failure && <div className="actions">
   <button type="button" className="secondary" onClick={() => setReloadKey(key => key + 1)}>重新读取</button>
  </div>}
  {value !== undefined && <form onSubmit={event => { event.preventDefault(); void handleSubmit(); }}>
   <div className="field" role="group" aria-label="这个主题的邮件设置">
    {OPTIONS.map(option => <label className="checkbox" key={option.value} title={option.hint}>
     <input
      type="radio"
      name="topic-email-override"
      value={option.value}
      checked={value === option.value}
      disabled={saving}
      onChange={() => { setValue(option.value); setNotice(undefined); }}
     />
     {option.label}
    </label>)}
    <p className="hint">只影响这个主题的邮件；站内通知始终照常。</p>
   </div>
   <Status>{notice}</Status>
   <div className="actions">
    <button type="submit" disabled={saving || value === saved}>{saving ? '正在保存…' : '保存这个主题的设置'}</button>
   </div>
  </form>}
 </section>;
}
