import { useEffect, useState } from 'react';
import { getHealth } from '../api/client';
import type { CurrentUser } from '../api/auth';
import type { Failure } from '../lib/messages';
import { Alert, Status } from '../components/Alert';
import { formatDateTime } from '../lib/format';

type HomeViewProps = {
 user?: CurrentUser;
 /** False until the first GET /api/v1/me has settled. */
 sessionReady: boolean;
 onLogout: () => Promise<Failure | undefined>;
};

export function HomeView({ user, sessionReady, onLogout }: HomeViewProps) {
 const [status, setStatus] = useState<'loading' | 'online' | 'offline'>('loading');
 const [attempt, setAttempt] = useState(0);
 const [logoutPending, setLogoutPending] = useState(false);
 const [logoutFailure, setLogoutFailure] = useState<Failure>();

 useEffect(() => {
  const controller = new AbortController();
  setStatus('loading');
  getHealth(controller.signal).then(() => setStatus('online')).catch(() => {
   if (!controller.signal.aborted) setStatus('offline');
  });
  return () => controller.abort();
 }, [attempt]);

 async function handleLogout() {
  if (logoutPending) return;
  setLogoutPending(true);
  setLogoutFailure(undefined);
  const failure = await onLogout();
  setLogoutFailure(failure);
  setLogoutPending(false);
 }

 const muteUntil = formatDateTime(user?.mute_expires_at);

 return <main>
  <p className="eyebrow">OPENTALK</p>
  <h1>交流想法，找到答案。</h1>
  <p>一个支持讨论、问答与匿名交流的小型社区。</p>
  <section aria-label="开发预览状态">
   <h2>开发预览</h2>
   <p>论坛功能正在建设中，当前页面用于确认前后端连接。</p>
   <p role="status">{status === 'loading' ? '正在连接服务…' : status === 'online' ? '已连接 API，业务功能尚未上线。' : '暂时无法连接服务，请稍后重试。'}</p>
   {status === 'offline' && <button onClick={() => setAttempt(n => n + 1)}>重新连接</button>}
  </section>
  <section aria-labelledby="topics-heading">
   <h2 id="topics-heading">主题列表</h2>
   <p>浏览全部板块的帖子与问题，可按板块筛选、按最新或最近活跃排序。</p>
   <nav className="links" aria-label="主题入口">
    <a href="#/topics">查看主题列表</a>
   </nav>
  </section>
  <section aria-labelledby="account-heading">
   <h2 id="account-heading">账号</h2>
   {!sessionReady && <Status>正在确认登录状态…</Status>}
   {sessionReady && user && <>
    <p>当前登录：{user.profile.nickname}</p>
    <nav className="links" aria-label="账号操作">
     <a href="#/me">我的资料</a>
     <button onClick={handleLogout} disabled={logoutPending}>{logoutPending ? '正在退出…' : '退出登录'}</button>
    </nav>
    {user.account_status === 'banned' && <Status>该账号已被封禁，不能再发布内容。</Status>}
    {user.is_muted && <Status>{muteUntil ? `该账号处于禁言期，截止 ${muteUntil}。` : '该账号处于禁言期。'}</Status>}
    <Alert failure={logoutFailure} />
   </>}
   {sessionReady && !user && <>
    <p>尚未登录。注册或登录后可以查看和编辑个人资料。</p>
    <nav className="links" aria-label="账号入口">
     <a href="#/register">注册</a>
     <a href="#/login">登录</a>
     <a href="#/me">我的资料</a>
    </nav>
   </>}
  </section>
 </main>;
}
