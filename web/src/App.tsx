import { useCallback, useEffect, useState } from 'react';
import { getCSRF, getMe, logout as logoutRequest, type CurrentUser } from './api/auth';
import { APIError, setCSRFToken } from './api/client';
import { useHashRoute } from './hooks/useHashRoute';
import { describeError, type Failure } from './lib/messages';
import { AdminSettingsView } from './views/AdminSettingsView';
import { HomeView } from './views/HomeView';
import { LoginView } from './views/LoginView';
import { ProfileView } from './views/ProfileView';
import { RegisterView } from './views/RegisterView';
import { ResetPasswordView } from './views/ResetPasswordView';

export function App() {
 const [route, navigate] = useHashRoute();
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

 // The hand-off text belongs to the login screen only.
 useEffect(() => {
  if (route !== 'login') setNotice(undefined);
 }, [route]);

 const handleAuthenticated = useCallback((current: CurrentUser) => {
  setUser(current);
  setNotice(undefined);
  navigate('me');
 }, [navigate]);

 const handleUserChange = useCallback((current: CurrentUser) => {
  setUser(current);
 }, []);

 const handleUnauthenticated = useCallback(() => {
  setUser(undefined);
  setNotice('请先登录后再查看资料。');
  navigate('login');
 }, [navigate]);

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
  navigate('login');
 }, [navigate]);

 const handleReset = useCallback((email: string) => {
  setLoginEmail(email);
  setNotice('密码已重置，请使用新密码登录。');
  navigate('login');
 }, [navigate]);

 switch (route) {
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
  default:
   return <HomeView user={user} sessionReady={sessionReady} onLogout={handleLogout} />;
 }
}
