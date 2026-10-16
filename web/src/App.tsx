import { useCallback, useEffect, useState } from 'react';
import { getCSRF, getMe, logout as logoutRequest, type CurrentUser } from './api/auth';
import { APIError, setCSRFToken, setUnauthenticatedHandler } from './api/client';
import { topicIdOf } from './api/content';
import { useHashRoute } from './hooks/useHashRoute';
import { describeError, type Failure } from './lib/messages';
import { dismissLinkConfirmation, LinkConfirmDialog } from './components/LinkConfirmDialog';
import { AdminSettingsView } from './views/AdminSettingsView';
import { HomeView } from './views/HomeView';
import { LoginView } from './views/LoginView';
import { ProfileView } from './views/ProfileView';
import { RegisterView } from './views/RegisterView';
import { ResetPasswordView } from './views/ResetPasswordView';
import { AdminModerationView } from './views/AdminModerationView';
import { AdminOperationsView } from './views/AdminOperationsView';
import { MyReportsView } from './views/MyReportsView';
import { NotificationsView } from './views/NotificationsView';
import { MyContentsView } from './views/MyContentsView';
import { UserProfileView } from './views/UserProfileView';
import { SearchView } from './views/SearchView';
import { TopicDetailView } from './views/TopicDetailView';
import { TopicEditorView, type EditorSaveResult } from './views/TopicEditorView';
import { TopicsView } from './views/TopicsView';
import { UnsubscribeView } from './views/UnsubscribeView';

export function App() {
 const [location, navigate] = useHashRoute();
 const [user, setUser] = useState<CurrentUser>();
 const [sessionReady, setSessionReady] = useState(false);
 // Hand-off text between views (注册成功 / 密码已重置 / 需要登录).
 const [notice, setNotice] = useState<string>();
 const [loginEmail, setLoginEmail] = useState('');

 // Session bootstrap: if the cookie is still valid, GET /api/v1/me says who we
 // are and GET /api/v1/auth/csrf gives us the in-memory token that a reloaded
 // page needs before it can perform writes. The token is never persisted.
 useEffect(() => {
  const controller = new AbortController();
  const bootstrap = async () => {
   try {
    const current = await getMe(controller.signal);
    if (controller.signal.aborted) return;
    setUser(current);
    try {
     const { csrf_token } = await getCSRF();
     if (!controller.signal.aborted) setCSRFToken(csrf_token);
    } catch {
     // Reads still work; a write will report CSRF_REJECTED with a retry hint.
    }
   } catch {
    if (!controller.signal.aborted) setUser(undefined);
   } finally {
    if (!controller.signal.aborted) setSessionReady(true);
   }
  };
  void bootstrap();
  return () => controller.abort();
 }, []);

 /**
  * A 401 anywhere in the API layer means the session is gone. Registering this
  * handler is what makes the upload path (which cannot go through `request`)
  * and the topic pages reset the session instead of leaving the UI signed in.
  */
 const handleUnauthenticated = useCallback(() => {
  setUser(undefined);
  setNotice('请先登录后再继续操作。');
  navigate({ name: 'login' });
 }, [navigate]);

 useEffect(() => {
  setUnauthenticatedHandler(handleUnauthenticated);
  return () => setUnauthenticatedHandler(undefined);
 }, [handleUnauthenticated]);

 // The hand-off text belongs to the login screen only.
 useEffect(() => {
  if (location.name !== 'login') setNotice(undefined);
 }, [location.name]);

 const handleAuthenticated = useCallback((current: CurrentUser) => {
  setUser(current);
  setNotice(undefined);
  navigate({ name: 'me' });
 }, [navigate]);

 const handleUserChange = useCallback((current: CurrentUser) => {
  setUser(current);
 }, []);

 const handleLogout = useCallback(async (): Promise<Failure | undefined> => {
  try {
   await logoutRequest();
   setCSRFToken(undefined);
   setUser(undefined);
   setNotice(undefined);
   return undefined;
  } catch (error) {
   if (error instanceof APIError && error.status === 401) {
    // The session was already gone server-side: treat it as signed out.
    setCSRFToken(undefined);
    setUser(undefined);
    return undefined;
   }
   // Keep the session visible: it may still be alive server-side.
   return describeError(error);
  }
 }, []);

 const handleRegistered = useCallback((email: string) => {
  setLoginEmail(email);
  setNotice('注册成功，请使用该邮箱和刚设置的密码登录。');
  navigate({ name: 'login' });
 }, [navigate]);

 const handleReset = useCallback((email: string) => {
  setLoginEmail(email);
  setNotice('密码已重置，请使用新密码登录。');
  navigate({ name: 'login' });
 }, [navigate]);

 /**
  * Follow the hash the editor reports. A topic opens its own detail page; an
  * answer or a comment — which has no page of its own — opens the discussion it
  * belongs to, which the editor passes as the fallback.
  */
 const handleSaved = useCallback((result: EditorSaveResult, fallbackTopicId?: string) => {
  const target = topicIdOf(result.content) ?? fallbackTopicId;
  const href = target === undefined || target === '' ? '#/' : `#/topic/${encodeURIComponent(target)}`;
  if (window.location.hash !== href) window.location.hash = href;
 }, []);

 /**
  * The one screen the current hash names. Kept as a function rather than a
  * return inside `switch` because every screen is rendered together with the
  * 外链确认 dialog below, which belongs to the app and not to a screen.
  */
 function currentScreen() {
  switch (location.name) {
   case 'login':
    return <LoginView notice={notice} initialEmail={loginEmail} onAuthenticated={handleAuthenticated} />;
   case 'register':
    return <RegisterView onRegistered={handleRegistered} />;
   case 'reset':
    return <ResetPasswordView onReset={handleReset} />;
   case 'me':
    return <ProfileView user={user} onUserChange={handleUserChange} onUnauthenticated={handleUnauthenticated} />;
   case 'admin':
    return <AdminSettingsView user={user} sessionReady={sessionReady} onUnauthenticated={handleUnauthenticated} />;
   case 'topics':
    return <TopicsView user={user} sessionReady={sessionReady} onUnauthenticated={handleUnauthenticated} />;
   case 'board':
    return <TopicsView user={user} sessionReady={sessionReady} boardId={location.id} onUnauthenticated={handleUnauthenticated} />;
   case 'tag':
    // 标签筛选复用主题列表：合同支持 tag_id 查询参数。
    return <TopicsView user={user} sessionReady={sessionReady} tagId={location.id} onUnauthenticated={handleUnauthenticated} />;
   case 'topic':
    return <TopicDetailView
     topicId={location.id}
     user={user}
     sessionReady={sessionReady}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'new':
    return <TopicEditorView
     initialBoardId={location.boardId}
     user={user}
     sessionReady={sessionReady}
     onSaved={handleSaved}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'edit':
    return <TopicEditorView
     contentId={location.id}
     user={user}
     sessionReady={sessionReady}
     onSaved={result => handleSaved(result, location.id)}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'admin-operations':
    return <AdminOperationsView
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     isAdmin={user?.role === 'admin'}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'search':
    return <SearchView
     q={location.q}
     boardId={location.boardId}
     tagId={location.tagId}
     user={user}
     sessionReady={sessionReady}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'me-reports':
    return <MyReportsView
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'me-notifications':
    return <NotificationsView
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'me-contents':
    return <MyContentsView
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'user':
    return <UserProfileView
     publicId={location.id}
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'admin-moderation':
    return <AdminModerationView
     sessionReady={sessionReady}
     signedIn={user !== undefined}
     isAdmin={user?.role === 'admin'}
     onUnauthenticated={handleUnauthenticated}
    />;
   case 'unsubscribe':
    // 邮件里的退订链接不要求登录，也不依赖会话状态：签名令牌就是全部凭据。
    // key 让另一个令牌（或刷新到一个失效令牌）从头开始，而不是沿用上一次的结果。
    return <UnsubscribeView key={location.token ?? ''} token={location.token} />;
   default:
    return <HomeView user={user} sessionReady={sessionReady} onLogout={handleLogout} />;
  }
 }

 /**
  * 继续访问 on the 外链中间页: the reader has seen the real domain and asked to
  * go there, so the navigation is handed to the browser exactly as the link
  * would have done it. The confirmation is closed first, so a slow destination
  * does not leave the dialog on screen.
  */
 const handleContinueToExternal = useCallback((href: string) => {
  dismissLinkConfirmation();
  window.location.assign(href);
 }, []);

 return <>
  {currentScreen()}
  <LinkConfirmDialog onContinue={handleContinueToExternal} />
 </>;
}
