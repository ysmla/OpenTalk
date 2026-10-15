import { useEffect, useState } from 'react';
import { listMyReports, type MyReport } from '../api/reports';
import { Alert, Status } from '../components/Alert';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type MyReportsViewProps = {
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

const STATUS_LABEL: Record<string, string> = {
 pending: '待处理',
 processing: '处理中',
 resolved: '已处理',
 dismissed: '不成立',
};

/**
 * 我的举报。
 *
 * 举报者能看到自己提交了什么、目前是什么状态，以及结案说明——这是"举报之后发生了什么"的唯一出口，
 * 所以结案说明直接展示，而不是藏在展开区里。列表不含举报者以外的任何身份信息，这与服务端一致。
 */
export function MyReportsView({ sessionReady, signedIn, onUnauthenticated }: MyReportsViewProps) {
 const [reports, setReports] = useState<MyReport[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(true);
 const [loadingMore, setLoadingMore] = useState(false);
 const [failure, setFailure] = useState<Failure>();

 useEffect(() => {
  // Reading requires a session; asking the server for it would only produce a 401 the caller cannot act on.
  if (!sessionReady) return;
  if (!signedIn) {
   setLoading(false);
   onUnauthenticated();
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  listMyReports({}, controller.signal).then(page => {
   if (controller.signal.aborted) return;
   setReports(page.items);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  }).catch((error: unknown) => {
   if (!controller.signal.aborted) setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [sessionReady, signedIn, onUnauthenticated]);

 async function loadMore() {
  if (loadingMore || cursor === undefined) return;
  setLoadingMore(true);
  try {
   const page = await listMyReports({ cursor });
   setReports(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setLoadingMore(false);
  }
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>我的举报</h1>
  <p className="hint">举报提交给管理员处理，它不会自动隐藏或删除内容。这里能看到处理进度与结案说明。</p>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {!loading && reports.length === 0 && !failure && <p className="hint">你还没有提交过举报。</p>}
  {reports.length > 0 && <>
   <ul className="reports">
    {reports.map(report => <li key={report.id}>
     <p className="byline">
      <span className="chip">{STATUS_LABEL[report.status] ?? report.status}</span>
      <span className="time">{formatDateTime(report.created_at)}</span>
     </p>
     <p><strong>举报原因：</strong>{report.reason}</p>
     {report.details !== '' && <p className="hint">补充说明：{report.details}</p>}
     {/* 结案后才有内容；未结案时服务端根本不下发这个字段。 */}
     {report.resolution_note !== undefined && report.resolution_note !== '' &&
      <p><strong>处理说明：</strong>{report.resolution_note}</p>}
     {report.resolved_at !== undefined && <p className="hint">结案时间：{formatDateTime(report.resolved_at)}</p>}
     <p className="hint">目标内容 ID：{report.content_id}</p>
    </li>)}
   </ul>
   {hasMore && <div className="actions">
    <button type="button" className="secondary" disabled={loadingMore} onClick={() => void loadMore()}>
     {loadingMore ? '正在加载…' : '加载更多'}
    </button>
   </div>}
  </>}
 </main>;
}
