import { useRef, useState, type ChangeEvent, type DragEvent } from 'react';
import type { Attachment } from '../api/media';
import { uploadImage } from '../api/media';
import { formatBytes } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

export type UploadItem = {
 attachment: Attachment;
 /** The local file name, kept only so the author can tell uploads apart. */
 name: string;
};

/** An upload in flight, keyed by a local id so two same-named files stay apart. */
type PendingUpload = {
 id: number;
 name: string;
 percent: number;
};

/** A failed upload kept around so the author can retry it without re-picking. */
type FailedUpload = {
 id: number;
 file: File;
 failure: Failure;
};

type ImageUploaderProps = {
 images: UploadItem[];
 onChange: (next: UploadItem[]) => void;
 disabled?: boolean;
 /** Called after a successful upload, e.g. to insert the image into Markdown. */
 onUploaded?: (item: UploadItem) => void;
 /**
  * The server's per-content image limit, when the client happens to know it.
  * It is not part of any read contract the front end may call, so when it is
  * unknown the client must not invent one (never a hardcoded 9): the server's
  * own TOO_MANY_ATTACHMENTS message is what the author sees.
  */
 limit?: number;
};

const ACCEPTED = 'image/png,image/jpeg,image/gif,image/webp';

/**
 * Image upload by drop or picker. Progress is real request progress, which is
 * why the upload goes through `api/media.ts` rather than `request`. A failed
 * upload keeps its File so 重试 re-sends the same bytes without re-picking.
 */
export function ImageUploader({ images, onChange, disabled, onUploaded, limit }: ImageUploaderProps) {
 const [pending, setPending] = useState<PendingUpload[]>([]);
 const [failures, setFailures] = useState<FailedUpload[]>([]);
 const [dragging, setDragging] = useState(false);
 const input = useRef<HTMLInputElement>(null);
 const nextId = useRef(1);

 async function send(file: File, id: number) {
  setPending(current => [...current, { id, name: file.name, percent: 0 }]);
  setFailures(current => current.filter(entry => entry.id !== id));
  try {
   const attachment = await uploadImage(file, percent => {
    setPending(current => current.map(entry => entry.id === id ? { ...entry, percent } : entry));
   });
   const item: UploadItem = { attachment, name: file.name };
   setPending(current => current.filter(entry => entry.id !== id));
   onChange([...images, item]);
   onUploaded?.(item);
  } catch (error) {
   setPending(current => current.filter(entry => entry.id !== id));
   setFailures(current => current.some(entry => entry.id === id) ? current : [...current, { id, file, failure: describeError(error) }]);
  }
 }

 function accept(files: FileList | null) {
  if (!files || disabled) return;
  for (const file of Array.from(files)) {
   const id = nextId.current;
   nextId.current += 1;
   void send(file, id);
  }
 }

 function handleDrop(event: DragEvent<HTMLDivElement>) {
  event.preventDefault();
  setDragging(false);
  accept(event.dataTransfer.files);
 }

 function handlePick(event: ChangeEvent<HTMLInputElement>) {
  accept(event.target.files);
  // Allow picking the same file again after a failure.
  event.target.value = '';
 }

 function retry(entry: FailedUpload) {
  setFailures(current => current.filter(candidate => candidate.id !== entry.id));
  void send(entry.file, entry.id);
 }

 function remove(attachmentId: string) {
  onChange(images.filter(item => item.attachment.id !== attachmentId));
 }

 const limitHint = limit === undefined
  ? `已上传 ${images.length} 张；数量上限以服务端配置为准，超限时服务端会提示。`
  : `已上传 ${images.length} / ${limit} 张。`;

 return <div className="uploader">
  <div
   className={dragging ? 'dropzone dragging' : 'dropzone'}
   onDragOver={event => { event.preventDefault(); setDragging(true); }}
   onDragLeave={() => setDragging(false)}
   onDrop={handleDrop}
  >
   <p>把图片拖到这里，或</p>
   <div className="actions">
    <button type="button" className="secondary" disabled={disabled} onClick={() => input.current?.click()}>
     选择图片
    </button>
    <span className="hint">支持 PNG、JPEG、GIF、WebP；上传成功后会自动插入正文。</span>
   </div>
   <input
    ref={input}
    type="file"
    accept={ACCEPTED}
    multiple
    hidden
    onChange={handlePick}
   />
  </div>
  <p className="hint">{limitHint}</p>
  {pending.map(entry => <p className="status" role="status" key={entry.id}>
   正在上传 {entry.name}…{entry.percent}%
  </p>)}
  {failures.map(entry => <p className="alert" role="alert" key={entry.id}>
   {entry.file.name} 上传失败：{entry.failure.text}
   {entry.failure.detail && <span className="detail">{entry.failure.detail}</span>}
   <button
    type="button"
    className="secondary"
    disabled={disabled || pending.some(candidate => candidate.id === entry.id)}
    onClick={() => retry(entry)}
   >
    重试
   </button>
  </p>)}
  {images.length > 0 && <ul className="attachments">
   {images.map(item => <li key={item.attachment.id}>
    <img src={item.attachment.url} alt={`已上传的图片 ${item.name}`} />
    <span className="hint">{formatBytes(item.attachment.byte_size) ?? item.attachment.mime_type}</span>
    <button type="button" className="link" disabled={disabled} onClick={() => remove(item.attachment.id)}>
     移除
    </button>
   </li>)}
  </ul>}
 </div>;
}
