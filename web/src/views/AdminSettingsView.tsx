import { useEffect, useId, useState, type FormEvent } from 'react';
import {
 ADMIN_FIELD_LABELS,
 ADMIN_NUMERIC_BOUNDS,
 ADMIN_NUMERIC_FIELDS,
 getAdminSettings,
 updateAdminSettings,
 type AdminNumericField,
 type AdminSettings,
 type AdminUpdateSettingsRequest,
} from '../api/admin';
import type { CurrentUser } from '../api/auth';
import { APIError } from '../api/client';
import { Alert, Status } from '../components/Alert';
import { Field } from '../components/Field';
import { formatDateTime } from '../lib/format';
import { describeError } from '../lib/messages';

type AdminSettingsViewProps = {
 user?: CurrentUser;
 /** False until App's first GET /api/v1/me has settled. */
 sessionReady: boolean;
 onUnauthenticated: () => void;
};

type SmtpSecurity = AdminSettings['smtp_security'];

/** Form text state: numbers are held as strings so every keystroke is editable. */
type SettingsForm = {
 smtp_host: string;
 smtp_port: string;
 smtp_security: SmtpSecurity;
 smtp_username: string;
 smtp_from_address: string;
 smtp_from_name: string;
 number: Record<AdminNumericField, string>;
};

/**
 * What the administrator asked for on the write-only password field:
 * undefined = leave unchanged, null = clear, a string = set that value.
 */
type PasswordIntent = string | null | undefined;

type NumberFieldProps = {
 label: string;
 value: string;
 onChange: (value: string) => void;
 min: number;
 max: number;
 hint?: string;
 disabled?: boolean;
};

/** Labelled integer input; the bounds come from the API contract. */
function NumberField({ label, value, onChange, min, max, hint, disabled }: NumberFieldProps) {
 const id = useId();
 const hintId = hint ? `${id}-hint` : undefined;
 return <div className="field">
  <label htmlFor={id}>{label}</label>
  <input
   id={id}
   type="number"
   inputMode="numeric"
   min={min}
   max={max}
   step={1}
   value={value}
   disabled={disabled}
   aria-describedby={hintId}
   onChange={event => onChange(event.target.value)}
  />
  {hint && <p className="hint" id={hintId}>{hint}</p>}
 </div>;
}

const SECURITY_OPTIONS: { value: SmtpSecurity; label: string }[] = [
 { value: 'none', label: '无加密' },
 { value: 'starttls', label: 'STARTTLS' },
 { value: 'tls', label: '隐式 TLS' },
];

const smtpLabels = {
 host: 'SMTP 主机',
 port: 'SMTP 端口',
 security: '传输加密',
 username: 'SMTP 用户名',
 password: 'SMTP 密码',
 fromAddress: '发信地址',
 fromName: '发信人显示名',
} as const;

/**
 * Trimmed text from a text input. A cleared optional field is sent as an empty
 * string so the server can drop the stored value.
 */
function textValue(value: string): string | undefined {
 const trimmed = value.trim();
 return trimmed === '' ? undefined : trimmed;
}

/** Empty and absent mean the same thing to the server, so neither is a change. */
function optionalTextChanged(formValue: string, loadedValue?: string): boolean {
 return textValue(formValue) !== textValue(loadedValue ?? '');
}

function optionalTextPatch(formValue: string, loadedValue?: string): string | undefined {
 return optionalTextChanged(formValue, loadedValue) ? formValue.trim() : undefined;
}

/** Bytes → MiB, so the raw number stays readable. */
function formatMiB(bytes: number): string {
 return `${(bytes / 1048576).toFixed(2)} MiB`;
}

function formFrom(settings: AdminSettings): SettingsForm {
 return {
  smtp_host: settings.smtp_host ?? '',
  smtp_port: settings.smtp_port === undefined ? '' : String(settings.smtp_port),
  smtp_security: settings.smtp_security,
  smtp_username: settings.smtp_username ?? '',
  smtp_from_address: settings.smtp_from_address ?? '',
  smtp_from_name: settings.smtp_from_name ?? '',
  number: {
   captcha_ttl_seconds: String(settings.captcha_ttl_seconds),
   captcha_max_attempts: String(settings.captcha_max_attempts),
   email_code_ttl_seconds: String(settings.email_code_ttl_seconds),
   email_code_max_attempts: String(settings.email_code_max_attempts),
   email_code_resend_interval_seconds: String(settings.email_code_resend_interval_seconds),
   email_code_email_hourly_limit: String(settings.email_code_email_hourly_limit),
   email_code_ip_hourly_limit: String(settings.email_code_ip_hourly_limit),
   login_captcha_failure_threshold: String(settings.login_captcha_failure_threshold),
   media_max_bytes: String(settings.media_max_bytes),
   media_max_dimension: String(settings.media_max_dimension),
   media_max_per_content: String(settings.media_max_per_content),
   media_pending_ttl_hours: String(settings.media_pending_ttl_hours),
   media_orphan_cleanup_days: String(settings.media_orphan_cleanup_days),
  },
 };
}

/** Every numeric field whose input differs from the loaded value. */
function changedNumbers(form: SettingsForm, loaded: AdminSettings): AdminNumericField[] {
 return ADMIN_NUMERIC_FIELDS.filter(field => form.number[field].trim() !== String(loaded[field]));
}

function isNumericFieldChanged(form: SettingsForm, loaded: AdminSettings, field: AdminNumericField): boolean {
 return form.number[field].trim() !== String(loaded[field]);
}

/** Port 0 is rejected by the server, so there is no way to clear it here. */
function portChanged(form: SettingsForm, loaded: AdminSettings): boolean {
 if (form.smtp_port.trim() === '') return false;
 const port = Number(form.smtp_port);
 return Number.isInteger(port) && port !== loaded.smtp_port;
}

function isPasswordIntent(intent: PasswordIntent): boolean {
 return intent !== undefined;
}

/** The count shown next to 保存: everything the next PATCH would carry. */
function countPendingChanges(form: SettingsForm, loaded: AdminSettings, passwordIntent: PasswordIntent, savedPasswordSet: boolean): number {
 let count = 0;
 if (optionalTextChanged(form.smtp_host, loaded.smtp_host)) count += 1;
 if (portChanged(form, loaded)) count += 1;
 if (form.smtp_security !== loaded.smtp_security) count += 1;
 if (optionalTextChanged(form.smtp_username, loaded.smtp_username)) count += 1;
 if (optionalTextChanged(form.smtp_from_address, loaded.smtp_from_address)) count += 1;
 if (optionalTextChanged(form.smtp_from_name, loaded.smtp_from_name)) count += 1;
 count += changedNumbers(form, loaded).length;
 if (isPasswordIntent(passwordIntent)) count += 1;
 // A clear request against an unset password would be a no-op PATCH.
 if (passwordIntent === null && !savedPasswordSet) count -= 1;
 return count;
}

/**
 * Friendly copy for the API error codes this screen can actually receive. An
 * unmapped code keeps the shared fallback in lib/messages.ts, which still shows
 * the server's own message as the secondary line.
 */
const ADMIN_FRIENDLY: Record<string, string> = {
 FORBIDDEN: '当前账号不是管理员，无法修改站点设置。',
 UNAUTHENTICATED: '登录状态已失效，请重新登录后再试。',
 CSRF_REJECTED: '登录状态校验失败，请刷新页面后重新登录再试。',
 SETTING_INVALID: '有设置项超出允许范围，请按提示修改后重试。',
 REASON_REQUIRED: '请填写修改原因，它会写入审计日志。',
 INVALID_REQUEST: '请求内容不符合接口要求，请检查填写的内容。',
 DEPENDENCY_UNAVAILABLE: '依赖的服务暂时不可用（如数据库或缓存），请稍后重试。',
};

function describeAdminError(error: unknown) {
 const failure = describeError(error);
 const code = error instanceof APIError ? error.code : undefined;
 const text = code ? ADMIN_FRIENDLY[code] : undefined;
 return text ? { text, detail: failure.detail ?? failure.text } : failure;
}

export function AdminSettingsView({ user, sessionReady, onUnauthenticated }: AdminSettingsViewProps) {
 const [loaded, setLoaded] = useState<AdminSettings>();
 const [form, setForm] = useState<SettingsForm>();
 const [loading, setLoading] = useState(true);
 const [loadFailure, setLoadFailure] = useState<ReturnType<typeof describeAdminError>>();
 // Set when either the session role or a stale session leaves us without access.
 const [denied, setDenied] = useState(false);
 const [passwordIntent, setPasswordIntent] = useState<PasswordIntent>();
 const [reason, setReason] = useState('');
 const [saving, setSaving] = useState(false);
 const [saveFailure, setSaveFailure] = useState<ReturnType<typeof describeAdminError>>();
 const [notice, setNotice] = useState<string>();

 useEffect(() => {
  const controller = new AbortController();
  async function load() {
   setLoading(true);
   try {
    const settings = await getAdminSettings(controller.signal);
    if (controller.signal.aborted) return;
    setLoaded(settings);
    setForm(formFrom(settings));
    setPasswordIntent(undefined);
    setLoadFailure(undefined);
    setDenied(false);
   } catch (error) {
    if (controller.signal.aborted) return;
    if (error instanceof APIError && error.status === 403) {
     setDenied(true);
     return;
    }
    if (error instanceof APIError && error.status === 401) {
     onUnauthenticated();
     return;
    }
    setLoadFailure(describeAdminError(error));
   } finally {
    if (!controller.signal.aborted) setLoading(false);
   }
  }
  void load();
  return () => controller.abort();
 }, [onUnauthenticated]);

 const updatedAt = formatDateTime(loaded?.updated_at);
 const savedPasswordSet = loaded?.smtp_password_set ?? false;
 const pending = loaded && form ? countPendingChanges(form, loaded, passwordIntent, savedPasswordSet) : 0;
 const canSave = Boolean(loaded && form) && pending > 0 && reason.trim() !== '' && !saving;

 /** The PATCH body: only the fields that differ, plus the reason and any password intent. */
 function buildPatch(current: SettingsForm, saved: AdminSettings): AdminUpdateSettingsRequest {
  const patch: AdminUpdateSettingsRequest = { reason: reason.trim() };
  const host = optionalTextPatch(current.smtp_host, saved.smtp_host);
  if (host !== undefined) patch.smtp_host = host;
  const port = Number(current.smtp_port);
  if (current.smtp_port.trim() !== '' && Number.isInteger(port) && port !== saved.smtp_port) patch.smtp_port = port;
  if (current.smtp_security !== saved.smtp_security) patch.smtp_security = current.smtp_security;
  const username = optionalTextPatch(current.smtp_username, saved.smtp_username);
  if (username !== undefined) patch.smtp_username = username;
  const fromAddress = optionalTextPatch(current.smtp_from_address, saved.smtp_from_address);
  if (fromAddress !== undefined) patch.smtp_from_address = fromAddress;
  const fromName = optionalTextPatch(current.smtp_from_name, saved.smtp_from_name);
  if (fromName !== undefined) patch.smtp_from_name = fromName;
  // An omitted smtp_password means "leave the stored password alone".
  if (typeof passwordIntent === 'string' && passwordIntent !== '') patch.smtp_password = passwordIntent;
  if (passwordIntent === null) patch.smtp_password = '';
  for (const field of ADMIN_NUMERIC_FIELDS) {
   if (!isNumericFieldChanged(current, saved, field)) continue;
   const value = Number(current.number[field]);
   const { min, max } = ADMIN_NUMERIC_BOUNDS[field];
   // An empty or unparsable box is not a new value: leave it out of the PATCH.
   if (!Number.isInteger(value) || value < min || value > max) continue;
   patch[field] = value;
  }
  return patch;
 }

 /** Client mirror of the server bounds: reported before any request is sent. */
 function boundsProblem(current: SettingsForm): string | undefined {
  for (const field of ADMIN_NUMERIC_FIELDS) {
   const raw = current.number[field].trim();
   const { min, max } = ADMIN_NUMERIC_BOUNDS[field];
   const label = ADMIN_FIELD_LABELS[field];
   if (raw === '') return `请填写${label}（${min}–${max}）。`;
   const value = Number(raw);
   if (!Number.isInteger(value)) return `${label}需要填写整数（${min}–${max}）。`;
   if (value < min || value > max) return `${label}需在 ${min}–${max} 之间。`;
  }
  const port = current.smtp_port.trim();
  if (port !== '') {
   const value = Number(port);
   if (!Number.isInteger(value) || value < 1 || value > 65535) return `${smtpLabels.port}需在 1–65535 之间；端口无法通过本页面清除，如需清除请使用 cmd/admin -action clear-smtp。`;
  }
  return undefined;
 }

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (saving) return;
  if (!loaded || !form) return;
  const problem = boundsProblem(form);
  if (problem) {
   setSaveFailure({ text: problem });
   return;
  }
  const patch = buildPatch(form, loaded);
  if (Object.keys(patch).length <= 1) {
   setSaveFailure({ text: '没有需要保存的修改。' });
   return;
  }
  setSaving(true);
  setSaveFailure(undefined);
  setNotice(undefined);
  try {
   const settings = await updateAdminSettings(patch);
   // Show exactly what the server stored, not what we hoped it stored.
   setLoaded(settings);
   setForm(formFrom(settings));
   // The password is never echoed back: drop the typed value for good.
   setPasswordIntent(undefined);
   setReason('');
   setNotice('站点设置已保存。');
  } catch (error) {
   if (error instanceof APIError && error.status === 403) {
    setDenied(true);
    return;
   }
   if (error instanceof APIError && error.status === 401) {
    onUnauthenticated();
    return;
   }
   setSaveFailure(describeAdminError(error));
  } finally {
   setSaving(false);
  }
 }

 if (denied || (sessionReady && user?.role !== 'admin')) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>后台管理</h1>
   <Alert failure={{ text: '当前账号不是管理员，无法进入后台管理。' }} />
   <nav className="links" aria-label="返回">
    <a href="#/">返回首页</a>
    <a href="#/me">返回我的资料</a>
   </nav>
  </main>;
 }

 if (loading) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>后台管理</h1>
   <Status>正在加载站点设置…</Status>
  </main>;
 }

 if (!loaded || !form) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>后台管理</h1>
   <Alert failure={loadFailure ?? { text: '站点设置加载失败，请稍后重试。' }} />
   <nav className="links" aria-label="返回">
    <a href="#/">返回首页</a>
    <a href="#/me">返回我的资料</a>
   </nav>
  </main>;
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>后台管理</h1>
  <p>修改只提交有改动的项，并记录修改原因。保存成功后，页面会重新读取服务端的实际值。</p>
  <dl className="meta">
   <dt>最近保存</dt>
   <dd>{updatedAt ?? '未知'}</dd>
   <dt>待提交修改</dt>
   <dd>{pending} 项</dd>
  </dl>
  <form onSubmit={handleSubmit} noValidate>
   <section aria-labelledby="admin-smtp-heading">
    <h2 id="admin-smtp-heading">邮件发送（SMTP）</h2>
    <div className="field">
     <label htmlFor="admin-smtp-host">{smtpLabels.host}</label>
     <input
      id="admin-smtp-host"
      value={form.smtp_host}
      autoComplete="off"
      spellCheck={false}
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_host: event.target.value })}
     />
     <p className="hint">留空并保存会清除已保存的主机；若已配置发信，请先确认新设置可用。</p>
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-port">{smtpLabels.port}</label>
     <input
      id="admin-smtp-port"
      type="number"
      inputMode="numeric"
      min={1}
      max={65535}
      step={1}
      value={form.smtp_port}
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_port: event.target.value })}
     />
     <p className="hint">
      端口范围 1–65535。本接口不接受端口 0，因此端口无法在这里清空（未配置时会省略）；需要彻底清除 SMTP 配置请执行
      cmd/admin -action clear-smtp。
     </p>
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-security">{smtpLabels.security}</label>
     <select
      id="admin-smtp-security"
      value={form.smtp_security}
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_security: event.target.value as SmtpSecurity })}
     >
      {SECURITY_OPTIONS.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
     </select>
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-username">{smtpLabels.username}</label>
     <input
      id="admin-smtp-username"
      value={form.smtp_username}
      autoComplete="off"
      spellCheck={false}
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_username: event.target.value })}
     />
     <p className="hint">留空并保存会清除已保存的用户名。</p>
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-password-input">{smtpLabels.password}</label>
     <input
      id="admin-smtp-password-input"
      type="password"
      value={typeof passwordIntent === 'string' ? passwordIntent : ''}
      autoComplete="new-password"
      disabled={saving}
      aria-describedby="admin-smtp-password-hint"
      onChange={event => {
       const next = event.target.value;
       setPasswordIntent(next === '' ? undefined : next);
      }}
     />
     <p className="hint" role="status" id="admin-smtp-password-hint">
      {savedPasswordSet ? '已保存密码（不会回显）。' : '尚未保存密码。'}密码只写不读：留空表示保持不变，
      只有填入新值或点击“清除已保存的密码”才会改动。
     </p>
    </div>
    <div className="actions">
     <button
      type="button"
      className="secondary"
      aria-pressed={passwordIntent === null}
      disabled={saving || (!savedPasswordSet && passwordIntent !== null)}
      onClick={() => setPasswordIntent(passwordIntent === null ? undefined : null)}
     >
      清除已保存的密码
     </button>
     {passwordIntent === null && <span className="hint" role="status">已标记：保存后将清除密码。</span>}
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-from-address">{smtpLabels.fromAddress}</label>
     <input
      id="admin-smtp-from-address"
      type="email"
      value={form.smtp_from_address}
      autoComplete="off"
      spellCheck={false}
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_from_address: event.target.value })}
     />
     <p className="hint">留空并保存会清除已保存的发信地址。</p>
    </div>
    <div className="field">
     <label htmlFor="admin-smtp-from-name">{smtpLabels.fromName}</label>
     <input
      id="admin-smtp-from-name"
      value={form.smtp_from_name}
      autoComplete="off"
      disabled={saving}
      onChange={event => setForm({ ...form, smtp_from_name: event.target.value })}
     />
     <p className="hint">留空并保存会清除已保存的显示名。</p>
    </div>
   </section>
   <section aria-labelledby="admin-code-heading">
    <h2 id="admin-code-heading">验证码与限流</h2>
    <NumberField
     label={ADMIN_FIELD_LABELS.captcha_ttl_seconds}
     value={form.number.captcha_ttl_seconds}
     onChange={value => setForm({ ...form, number: { ...form.number, captcha_ttl_seconds: value } })}
     min={ADMIN_NUMERIC_BOUNDS.captcha_ttl_seconds.min}
     max={ADMIN_NUMERIC_BOUNDS.captcha_ttl_seconds.max}
     hint="单位：秒（60–1800）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.captcha_max_attempts}
     value={form.number.captcha_max_attempts}
     onChange={value => setForm({ ...form, number: { ...form.number, captcha_max_attempts: value } })}
     min={ADMIN_NUMERIC_BOUNDS.captcha_max_attempts.min}
     max={ADMIN_NUMERIC_BOUNDS.captcha_max_attempts.max}
     hint="范围 1–10。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.email_code_ttl_seconds}
     value={form.number.email_code_ttl_seconds}
     onChange={value => setForm({ ...form, number: { ...form.number, email_code_ttl_seconds: value } })}
     min={ADMIN_NUMERIC_BOUNDS.email_code_ttl_seconds.min}
     max={ADMIN_NUMERIC_BOUNDS.email_code_ttl_seconds.max}
     hint="单位：秒（60–3600）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.email_code_max_attempts}
     value={form.number.email_code_max_attempts}
     onChange={value => setForm({ ...form, number: { ...form.number, email_code_max_attempts: value } })}
     min={ADMIN_NUMERIC_BOUNDS.email_code_max_attempts.min}
     max={ADMIN_NUMERIC_BOUNDS.email_code_max_attempts.max}
     hint="范围 1–10。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.email_code_resend_interval_seconds}
     value={form.number.email_code_resend_interval_seconds}
     onChange={value => setForm({ ...form, number: { ...form.number, email_code_resend_interval_seconds: value } })}
     min={ADMIN_NUMERIC_BOUNDS.email_code_resend_interval_seconds.min}
     max={ADMIN_NUMERIC_BOUNDS.email_code_resend_interval_seconds.max}
     hint="单位：秒（10–600）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.email_code_email_hourly_limit}
     value={form.number.email_code_email_hourly_limit}
     onChange={value => setForm({ ...form, number: { ...form.number, email_code_email_hourly_limit: value } })}
     min={ADMIN_NUMERIC_BOUNDS.email_code_email_hourly_limit.min}
     max={ADMIN_NUMERIC_BOUNDS.email_code_email_hourly_limit.max}
     hint="范围 1–100（每小时）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.email_code_ip_hourly_limit}
     value={form.number.email_code_ip_hourly_limit}
     onChange={value => setForm({ ...form, number: { ...form.number, email_code_ip_hourly_limit: value } })}
     min={ADMIN_NUMERIC_BOUNDS.email_code_ip_hourly_limit.min}
     max={ADMIN_NUMERIC_BOUNDS.email_code_ip_hourly_limit.max}
     hint="范围 1–1000（每小时）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.login_captcha_failure_threshold}
     value={form.number.login_captcha_failure_threshold}
     onChange={value => setForm({ ...form, number: { ...form.number, login_captcha_failure_threshold: value } })}
     min={ADMIN_NUMERIC_BOUNDS.login_captcha_failure_threshold.min}
     max={ADMIN_NUMERIC_BOUNDS.login_captcha_failure_threshold.max}
     hint="范围 1–50。"
     disabled={saving}
    />
   </section>
   <section aria-labelledby="admin-media-heading">
    <h2 id="admin-media-heading">图片限制</h2>
    <NumberField
     label={ADMIN_FIELD_LABELS.media_max_bytes}
     value={form.number.media_max_bytes}
     onChange={value => setForm({ ...form, number: { ...form.number, media_max_bytes: value } })}
     min={ADMIN_NUMERIC_BOUNDS.media_max_bytes.min}
     max={ADMIN_NUMERIC_BOUNDS.media_max_bytes.max}
     hint={`单位：字节（65536–104857600）。当前约 ${formatMiB(Number(form.number.media_max_bytes) || 0)}。`}
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.media_max_dimension}
     value={form.number.media_max_dimension}
     onChange={value => setForm({ ...form, number: { ...form.number, media_max_dimension: value } })}
     min={ADMIN_NUMERIC_BOUNDS.media_max_dimension.min}
     max={ADMIN_NUMERIC_BOUNDS.media_max_dimension.max}
     hint="宽和高的像素上限（256–8192）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.media_max_per_content}
     value={form.number.media_max_per_content}
     onChange={value => setForm({ ...form, number: { ...form.number, media_max_per_content: value } })}
     min={ADMIN_NUMERIC_BOUNDS.media_max_per_content.min}
     max={ADMIN_NUMERIC_BOUNDS.media_max_per_content.max}
     hint="范围 1–30。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.media_pending_ttl_hours}
     value={form.number.media_pending_ttl_hours}
     onChange={value => setForm({ ...form, number: { ...form.number, media_pending_ttl_hours: value } })}
     min={ADMIN_NUMERIC_BOUNDS.media_pending_ttl_hours.min}
     max={ADMIN_NUMERIC_BOUNDS.media_pending_ttl_hours.max}
     hint="单位：小时（1–720）。"
     disabled={saving}
    />
    <NumberField
     label={ADMIN_FIELD_LABELS.media_orphan_cleanup_days}
     value={form.number.media_orphan_cleanup_days}
     onChange={value => setForm({ ...form, number: { ...form.number, media_orphan_cleanup_days: value } })}
     min={ADMIN_NUMERIC_BOUNDS.media_orphan_cleanup_days.min}
     max={ADMIN_NUMERIC_BOUNDS.media_orphan_cleanup_days.max}
     hint="单位：天（1–90）。"
     disabled={saving}
    />
   </section>
   <section aria-labelledby="admin-reason-heading">
    <h2 id="admin-reason-heading">修改原因</h2>
    <div className="field">
     <label htmlFor="admin-reason">修改原因（必填）</label>
     <input
      id="admin-reason"
      value={reason}
      autoComplete="off"
      disabled={saving}
      aria-describedby="admin-reason-hint"
      onChange={event => setReason(event.target.value)}
     />
     <p className="hint" id="admin-reason-hint">会写入审计日志；请勿在此填写密码或密钥，审计只记录改动的字段名。</p>
    </div>
   </section>
   <Status>{notice}</Status>
   <Alert failure={saveFailure} />
   <div className="actions">
    <button type="submit" disabled={!canSave}>{saving ? '正在保存…' : '保存'}</button>
    <span className="hint" role="status">
     {pending > 0 ? `待提交修改：${pending} 项。` : '尚未修改任何设置。'}
    </span>
    <a href="#/me">返回我的资料</a>
    <a href="#/">返回首页</a>
   </div>
  </form>
 </main>;
}
