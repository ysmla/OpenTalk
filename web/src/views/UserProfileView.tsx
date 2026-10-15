import { useEffect, useState } from 'react';
import { getPublicUser, type PublicUser } from '../api/profiles';
import { Alert, Status } from '../components/Alert';
import { ContentListView } from './MyContentsView';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type UserProfileViewProps = {
 publicId: string;
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

/**
 * 公开用户主页：`#/user/{public_id}`。
 *
 * 资料只含公开字段（服务端不下发邮箱、内部 ID、角色或账号状态），内容列表**不含匿名帖**——那不是界面
 * 过滤的，服务端在 SQL 里就排除了。这里因此没有"显示/隐藏匿名"之类的开关可做，也不该有。
 */
export function UserProfileView({ publicId, sessionReady, signedIn, onUnauthenticated }: UserProfileViewProps) {
 const [profile, setProfile] = useState<PublicUser>();
 const [loading, setLoading] = useState(true);
 const [failure, setFailure] = useState<Failure>();

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  setProfile(undefined);
  getPublicUser(publicId, controller.signal).then(user => {
   if (!controller.signal.aborted) setProfile(user);
  }).catch((error: unknown) => {
   if (!controller.signal.aborted) setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [publicId]);

 const joined = profile ? formatDateTime(profile.created_at) : '';
 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {profile && <>
   <h1>{profile.nickname}</h1>
   <p className="hint">
    {profile.avatar_url !== undefined && <img className="avatar" src={profile.avatar_url} alt="" width={48} height={48} />}
    公开 ID：{profile.public_id}{joined !== '' && <>　注册于 {joined}</>}
   </p>
   {profile.bio !== undefined && profile.bio !== '' && <p>{profile.bio}</p>}
   <h2>公开内容</h2>
   <p className="hint">这里只列出该用户的公开内容；匿名发言不出现在任何人的主页上。</p>
   <ContentListView
    publicId={publicId}
    sessionReady={sessionReady}
    signedIn={signedIn}
    onUnauthenticated={onUnauthenticated}
   />
  </>}
 </main>;
}
