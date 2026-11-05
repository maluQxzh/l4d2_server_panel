import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, Download, File, FileCode2, Folder, Save, ShieldAlert, Trash2, Upload } from "lucide-react";
import { useEffect, useState } from "react";
import { FileUploadInput } from "../components/FileUploadInput";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useToast } from "../context/ToastContext";
import { hasInstancePermission } from "../lib/account-permissions";
import { api } from "../lib/api";
import { bytes, dateTime } from "../lib/format";
import { refreshQueries } from "../lib/query-refresh";
import type { InstanceRecord } from "../lib/types";
import { bytesToBase64, OptionalUnavailable } from './InstanceDetailShared';

export function FilesTab({ instance }: { instance: InstanceRecord }) {
  const { user } = useAuth();
  const [path, setPath] = useState("");
  const [editor, setEditor] = useState<{ path: string; isNew: boolean } | null>(null);
  const [serverShortcut, setServerShortcut] = useState(false);
  const canList = hasInstancePermission(user, instance, "files.list");
  const canRead = hasInstancePermission(user, instance, "files.read");
  const canWrite = hasInstancePermission(user, instance, "files.write");
  const canDelete = hasInstancePermission(user, instance, "files.delete");
  const canOpenFile = canRead || canWrite || canDelete;
  const files = useQuery({
    queryKey: ["instances", instance.id, "files", path],
    queryFn: () => api.files(instance.id, path),
    enabled: canList,
    refetchInterval: (query) => query.state.data?.status === "loading" || query.state.data?.refresh_job_id ? 750 : 10_000,
  });
  const allowedRoots = [
    { path: "left4dead2/addons", label: "addons" },
    { path: "left4dead2/cfg", label: "cfg" },
  ];
  const adminsPath = "left4dead2/addons/sourcemod/configs/admins_simple.ini";
  const adminsShortcut = files.data?.shortcuts.some((item) => item.type === "file" && item.path.toLowerCase() === adminsPath);
  const refreshingFiles = files.data?.status === "loading" || Boolean(files.data?.refresh_job_id);
  useEffect(() => {
    setPath("");
    setEditor(null);
    setServerShortcut(false);
  }, [instance.id]);
  useEffect(() => {
    if (!serverShortcut || path !== "left4dead2/cfg" || files.isPending || files.error || refreshingFiles) return;
    const serverPath = "left4dead2/cfg/server.cfg";
    const exists = files.data.items.some((item) => item.type === "file" && item.path.toLowerCase() === serverPath);
    if (exists ? canOpenFile : canWrite) setEditor({ path: serverPath, isNew: !exists });
    setServerShortcut(false);
  }, [canOpenFile, canWrite, files.data, files.error, files.isPending, path, refreshingFiles, serverShortcut]);
  if (canList && files.isPending) return <LoadingBlock label="正在读取实例目录…" />;
  if (files.error) return <OptionalUnavailable error={files.error} title="文件浏览暂不可用" description="当前控制端未开放受限文件列表接口。Agent 仍会强制执行实例根目录边界。" />;
  const newPath = `${path || "left4dead2/cfg"}/new-file.cfg`;
  const createButton = canWrite ? <Button variant="secondary" onClick={() => setEditor({ path: newPath, isNew: true })}><Upload size={16} />上传或新建</Button> : undefined;
  return <>
    <Card>
      <div className="card-heading"><div><span className="eyebrow">SANDBOXED FILES</span><h2>文件管理</h2></div>{createButton}</div>
      {!canList ? <EmptyState title="文件目录已隐藏" description="当前账号没有浏览目录权限，但仍可向已知安全路径执行单独授予的写入操作。" action={createButton} /> : <div className="file-browser">
        <aside>
          {allowedRoots.map((root) => <button className={path === root.path ? "active" : ""} onClick={() => setPath(root.path)} key={root.path}><Folder size={15} />{root.label}</button>)}
          <button disabled={!canOpenFile && !canWrite} onClick={() => { setServerShortcut(true); setPath("left4dead2/cfg"); }}><FileCode2 size={15} />server.cfg</button>
          {adminsShortcut && <button disabled={!canOpenFile} onClick={() => setEditor({ path: adminsPath, isNew: false })}><FileCode2 size={15} />admins_simple.ini</button>}
        </aside>
        <div className="file-list">
          <div className="file-path"><button onClick={() => setPath("")}>根目录</button>{path.split("/").filter(Boolean).map((part, index, all) => <span key={`${part}-${index}`}><ChevronRight size={13} /><button onClick={() => setPath(all.slice(0, index + 1).join("/"))}>{part}</button></span>)}</div>
          {refreshingFiles && !files.data?.items.length ? <LoadingBlock label="正在读取目录内容…" /> : files.data?.items.length ? files.data.items.map((item) => <button className="file-row" key={item.path} disabled={item.type === "file" && !canOpenFile} onClick={() => item.type === "directory" ? setPath(item.path) : setEditor({ path: item.path, isNew: false })}><span className={`file-type file-type--${item.type}`}>{item.type === "directory" ? <Folder size={18} /> : <File size={18} />}</span><strong>{item.name}</strong><span>{item.type === "file" ? bytes(item.size) : "文件夹"}</span><time>{dateTime(item.modified_at)}</time><ChevronRight size={15} /></button>) : <EmptyState title="目录为空" description="这个受限目录中还没有文件。" />}
        </div>
      </div>}
    </Card>
    <FileEditorModal instance={instance} editor={editor} onClose={() => { setEditor(null); if (canList) void files.refetch(); }} />
  </>;
}

export type FileEditorTarget = { path: string; isNew: boolean; packageId?: string; packageName?: string };

export function FileEditorModal({ instance, editor, onClose }: { instance: InstanceRecord; editor: FileEditorTarget | null; onClose: () => void }) {
  const { user } = useAuth();
  const toast = useToast();
  const client = useQueryClient();
  const [relative, setRelative] = useState("");
  const [content, setContent] = useState("");
  const [binary, setBinary] = useState<string | null>(null);
  const canRead = hasInstancePermission(user, instance, "files.read");
  const canWrite = hasInstancePermission(user, instance, "files.write");
  const canDelete = hasInstancePermission(user, instance, "files.delete") && !editor?.packageId;
  const file = useQuery({
    queryKey: ["instances", instance.id, "file-content", editor?.path],
    queryFn: () => editor?.packageId
      ? api.packageFileContent(instance.id, editor.packageId, editor.path)
      : api.fileContent(instance.id, editor?.path || ""),
    enabled: canRead && Boolean(editor && !editor.isNew),
    refetchInterval: (query) => query.state.data?.status === "loading" || query.state.data?.refresh_job_id ? 750 : false,
  });
  useEffect(() => {
    if (!editor) return;
    setRelative(editor.path);
    setContent("");
    setBinary(null);
  }, [editor]);
  useEffect(() => {
    if (!file.data || file.data.status === "loading" || file.data.refresh_job_id) return;
    if (file.data.content != null) {
      setContent(file.data.content);
      setBinary(null);
    } else if (file.data.content_base64 != null) {
      setBinary(file.data.content_base64);
    }
  }, [file.data]);
  const save = useMutation({
    mutationFn: () => editor?.packageId
      ? api.writePackageFile(instance.id, editor.packageId, relative, content, file.data?.sha256 || "")
      : api.writeFile(instance.id, relative, content, editor?.isNew ? undefined : file.data?.sha256, binary || undefined),
    onSuccess: (result) => {
      toast.push(editor?.packageId ? "插件文件更新任务已提交" : "文件写入任务已提交");
      void refreshQueries(client, { queryKey: ["jobs"] });
      onClose();
      return result;
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteFile(instance.id, relative, file.data?.sha256),
    onSuccess: () => { toast.push("文件删除任务已提交"); void refreshQueries(client, { queryKey: ["jobs"] }); onClose(); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const loadUpload = async (selected: File) => {
    if (selected.size > 8 * 1024 * 1024) { toast.push("单个文件不能超过 8 MiB", "error"); return; }
    setRelative(`${relative.split("/").slice(0, -1).join("/")}/${selected.name}`.replace(/^\//, ""));
    const raw = new Uint8Array(await selected.arrayBuffer());
    const text = new TextDecoder("utf-8", { fatal: false }).decode(raw);
    const looksTextual = !raw.includes(0) && !text.includes("�");
    if (looksTextual) { setContent(text); setBinary(null); } else { setContent(""); setBinary(bytesToBase64(raw)); }
  };
  const download = () => {
    const encoded = file.data?.content_base64;
    if (!encoded) return;
    const raw = Uint8Array.from(atob(encoded), (character) => character.charCodeAt(0));
    const url = URL.createObjectURL(new Blob([raw]));
    const anchor = document.createElement("a");
    anchor.href = url; anchor.download = relative.split("/").pop() || "download"; anchor.click();
    URL.revokeObjectURL(url);
  };
  const loading = Boolean(canRead && editor && !editor.isNew && (file.isPending || file.data?.status === "loading" || file.data?.refresh_job_id));
  return <Modal open={Boolean(editor)} onClose={onClose} title={editor?.packageId ? `编辑 ${editor.packageName || "插件"} 文件` : editor?.isNew ? "上传或新建文件" : canWrite ? "编辑受限文件" : "查看受限文件"} description={editor?.packageId ? "保存时会同步智能插件清单与校验哈希，入口文件和原生二进制保持只读。" : "Agent 会再次校验实例边界、重解析点和 8 MiB 大小限制。"} footer={<><Button variant="secondary" onClick={onClose}>取消</Button>{!editor?.isNew && canDelete && <Button variant="danger" onClick={() => { if (window.confirm(`确认删除 ${relative}？`)) remove.mutate(); }} loading={remove.isPending}><Trash2 size={15} />删除</Button>}{canWrite && <Button onClick={() => save.mutate()} loading={save.isPending} disabled={!relative || loading || Boolean(editor?.packageId && binary != null)}><Save size={16} />提交写入</Button>}</>}>
    {loading ? <LoadingBlock label="正在从节点安全读取文件…" /> : canRead && file.error ? <ErrorState error={file.error} retry={() => void file.refetch()} /> : <div className="form-stack"><Field label="实例内相对路径"><input value={relative} onChange={(event) => setRelative(event.target.value)} disabled={!editor?.isNew} /></Field>{!canRead && !editor?.isNew && <div className="form-note"><ShieldAlert size={17} />当前权限不允许读取文件内容；仍可执行单独授予的写入或删除操作。</div>}{editor?.isNew && canWrite && <Field label="从本机选择文件" hint="文本文件可继续编辑；二进制文件会以 Base64 安全传输"><FileUploadInput label="从本机选择文件" maxBytes={8 * 1024 * 1024} disabled={save.isPending || remove.isPending} onFile={loadUpload} /></Field>}{canRead || editor?.isNew ? binary != null ? <div className="binary-file-note"><File size={20} /><div><strong>二进制文件</strong><span>{binary.length ? `Base64 载荷约 ${bytes(Math.floor(binary.length * 0.75))}` : "空文件"}</span></div>{!editor?.isNew && <Button variant="secondary" onClick={download}><Download size={15} />下载</Button>}</div> : <Field label="文件内容" hint="保存使用读取时的 SHA-256 做并发保护，文件被外部修改时会拒绝覆盖。"><textarea className="file-editor" rows={18} value={content} onChange={(event) => setContent(event.target.value)} readOnly={!canWrite} spellCheck={false} /></Field> : null}{file.data?.sha256 && <div className="file-digest"><span>SHA-256</span><code>{file.data.sha256}</code></div>}</div>}
  </Modal>;
}
