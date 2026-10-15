import { useCallback, useEffect, useState } from 'react';
import { getCSRF, getMe, logout as logoutRequest, type CurrentUser } from './api/auth';
import { APIError, setCSRFToken, setUnauthenticatedHandler } from './api/client';
import { isTopic } from './api/content';
import { useHashRoute } from './hooks/useHashRoute';
import { describeError, type Failure } from './lib/messages';
import { AdminSettingsView } from './views/AdminSettingsView';
import { HomeView } from './views/HomeView';
import { LoginView } from './views/LoginView';
import { ProfileView } from './views/ProfileView';
import { RegisterView } from './views/RegisterView';
import { ResetPasswordView } from './views/ResetPasswordView';
import { MyReportsView } from './views/MyReportsView';
import { SearchView } from './views/SearchView';
import { TopicDetailView } from './views/TopicDetailView';
import { TopicEditorView, type EditorSaveResult } from './views/TopicEditorView';
import { TopicsView } from './views/TopicsView';

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
  * Follow the hash the server's own response points at. The editor reports what
  * the server created or returned, and this decides where to land: a topic
  * opens its detail page, anything else falls back to the caller's page.
  */
 const handleSaved = useCallback((result: EditorSaveResult, fallbackTopicId?: string) => {
  const target = isTopic(result.content) ? result.content.id : fallbackTopicId;
  const href = target === undefined || target === '' ? '#/' : `#/topic/${encodeURIComponent(target)}`;
  if (window.location.hash !== href) window.location.hash = href;
 }, []);

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
  default:
   return <HomeView user={user} sessionReady={sessionReady} onLogout={handleLogout} />;
 }
}
