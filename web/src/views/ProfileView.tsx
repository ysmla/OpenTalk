import { useEffect, useState, type FormEvent } from 'react';
import { getMe, updateMe, type CurrentUser, type UpdateProfileRequest } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { Field, TextAreaField } from '../components/Field';
import { MailPreferences } from '../components/MailPreferences';
import { formatDateTime } from '../lib/format';
import { describeError, isStatus, type Failure } from '../lib/messages';
import { bioProblem, charCount, nicknameProblem, BIO_MAX, NICKNAME_MAX } from '../lib/validate';

type ProfileViewProps = {
 user?: CurrentUser;
 onUserChange: (user: CurrentUser) => void;
 onUnauthenticated: () => void;
};

export function ProfileView({ user, onUserChange, onUnauthenticated }: ProfileViewProps) {
 const [profile, setProfile] = useState<CurrentUser | undefined>(user);
 const [nickname, setNickname] = useState(user?.profile.nickname ?? '');
 const [bio, setBio] = useState(user?.profile.bio ?? '');
 const [loading, setLoading] = useState(true);
 const [saving, setSaving] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  getMe(controller.signal).then(current => {
   if (controller.signal.aborted) return;
   setProfile(current);
   setNickname(current.profile.nickname);
   setBio(current.profile.bio ?? '');
   onUserChange(current);
   setFailure(undefined);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [onUserChange, onUnauthenticated]);

 const savedNickname = profile?.profile.nickname ?? '';
 const savedBio = profile?.profile.bio ?? '';
 const dirty = Boolean(profile) && (nickname !== savedNickname || bio !== savedBio);
 const muteUntil = formatDateTime(profile?.mute_expires_at);
 const createdAt = formatDateTime(profile?.profile.created_at);

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (saving) return;
  if (!profile) return;
  const problem = nicknameProblem(nickname) ?? bioProblem(bio);
  if (problem) {
   setFailure({ text: problem });
   return;
  }
  const patch: UpdateProfileRequest = {};
  if (nickname !== savedNickname) patch.nickname = nickname;
  if (bio !== savedBio) patch.bio = bio;
  if (Object.keys(patch).length === 0) {
   setFailure({ text: '没有需要保存的修改。' });
   return;
  }
  setSaving(true);
  setFailure(undefined);
  setNotice(undefined);
  try {
   const current = await updateMe(patch);
   setProfile(current);
   setNickname(current.profile.nickname);
   setBio(current.profile.bio ?? '');
   onUserChange(current);
   setNotice('资料已保存。');
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setFailure(describeError(error));
  } finally {
   setSaving(false);
  }
 }

 if (!profile) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>我的资料</h1>
   {loading
    ? <Status>正在加载资料…</Status>
    : <>
     <Status>需要登录后才能查看资料。</Status>
     <nav className="links" aria-label="账号入口">
      <a href="#/login">去登录</a>
      <a href="#/register">注册</a>
     </nav>
    </>}
   <Alert failure={failure} />
   <nav className="links" aria-label="返回">
    <a href="#/">返回首页</a>
   </nav>
  </main>;
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>我的资料</h1>
  <dl className="meta">
   <dt>邮箱</dt>
   <dd>{profile.email}</dd>
   <dt>用户 ID</dt>
   <dd>{profile.profile.public_id}</dd>
   <dt>注册时间</dt>
   <dd>{createdAt ?? '未知'}</dd>
  </dl>
  {profile.account_status === 'banned' && <Status>账号状态：已被封禁，无法发布内容。如有疑问请联系管理员。</Status>}
  {profile.is_muted && <Status>{muteUntil ? `账号状态：禁言中，截止 ${muteUntil}。` : '账号状态：禁言中。'}</Status>}
  {profile.account_status === 'active' && !profile.is_muted &&
   <Status>账号状态：正常{profile.role === 'admin' ? '（管理员）' : ''}。</Status>}
  {profile.role === 'admin' && <section aria-labelledby="admin-entry-heading">
   <h2 id="admin-entry-heading">管理员</h2>
   <p>当前账号具有管理员权限，可以修改站点设置。改动会写入审计日志。</p>
   <nav className="links" aria-label="管理员入口">
    <a href="#/admin">进入后台管理</a>
    <a href="#/admin/moderation">审核与举报</a>
    <a href="#/admin/operations">账号与站点</a>
   </nav>
  </section>}
  <section aria-labelledby="my-contents-heading">
   <h2 id="my-contents-heading">我的内容</h2>
   <p>我发布的帖子、回答与评论；我自己的匿名帖也会列在那里，并仍以匿名身份展示。</p>
   <nav className="links" aria-label="我的内容">
    <a href="#/me/contents">查看我的内容</a>
   </nav>
  </section>
  <section aria-labelledby="my-notifications-heading">
   <h2 id="my-notifications-heading">通知</h2>
   <p>别人回复你、回答你的提问或你的举报有结果时的站内通知；与邮件通知是两件事。</p>
   <nav className="links" aria-label="通知">
    <a href="#/me/notifications">查看通知</a>
   </nav>
  </section>
  {/* 邮件偏好是可写设置，不是说明文字：界面上的文案不等于有设置功能。 */}
  <MailPreferences signedIn={profile !== undefined} />
  <section aria-labelledby="my-reports-heading">
   <h2 id="my-reports-heading">我的举报</h2>
   <p>你提交过的举报与处理结果；举报不会自动隐藏或删除内容。</p>
   <nav className="links" aria-label="举报记录">
    <a href="#/me/reports">查看我的举报</a>
   </nav>
  </section>
  <form onSubmit={handleSubmit} noValidate>
   <Field
    label="昵称"
    autoComplete="nickname"
    value={nickname}
    onChange={setNickname}
    hint={`最多 ${NICKNAME_MAX} 个字符，仅限汉字或字母（当前 ${charCount(nickname)}）。`}
    disabled={saving || loading}
   />
   <TextAreaField
    label="个人简介"
    value={bio}
    onChange={setBio}
    hint={`最多 ${BIO_MAX} 个字符（当前 ${charCount(bio)}）。`}
    disabled={saving || loading}
   />
   <Status>{notice}</Status>
   <Alert failure={failure} />
   <div className="actions">
    <button type="submit" disabled={saving || loading || !dirty}>{saving ? '正在保存…' : '保存修改'}</button>
    <a href="#/">返回首页</a>
   </div>
  </form>
 </main>;
}
