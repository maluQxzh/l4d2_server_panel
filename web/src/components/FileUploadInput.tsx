import { Upload } from "lucide-react";
import { useEffect, useId, useRef, useState, type RefObject } from "react";
import { bytes } from "../lib/format";

type Props = {
  label: string;
  accept?: string;
  maxBytes: number;
  minBytes?: number;
  disabled?: boolean;
  fileName?: string;
  inputRef?: RefObject<HTMLInputElement | null>;
  onFile: (file: File) => void | Promise<void>;
};

function acceptedFile(file: File, accept: string) {
  const tokens = accept.toLowerCase().split(",").map((value) => value.trim()).filter(Boolean);
  const extensions = tokens.filter((value) => value.startsWith("."));
  // Browser MIME detection varies by OS. Where an extension list is provided,
  // use it for both the picker and drops; content validation still runs later.
  if (extensions.length) return extensions.some((extension) => file.name.toLowerCase().endsWith(extension));
  return !tokens.length || tokens.some((type) => type.endsWith("/*") ? file.type.toLowerCase().startsWith(type.slice(0, -1)) : file.type.toLowerCase() === type);
}

export function FileUploadInput({ label, accept = "", maxBytes, minBytes = 0, disabled = false, fileName, inputRef, onFile }: Props) {
  const ownRef = useRef<HTMLInputElement>(null);
  const ref = inputRef || ownRef;
  const descriptionId = useId();
  const errorId = useId();
  const depth = useRef(0);
  const processing = useRef(false);
  const [reading, setReading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [selected, setSelected] = useState("");
  const [error, setError] = useState("");
  const blocked = disabled || reading;

  useEffect(() => {
    // Prevent the browser from navigating away when a file misses the dropzone.
    // Text, URLs and existing text-editor drag behavior are unaffected.
    const preventFileNavigation = (event: DragEvent) => {
      if (Array.from(event.dataTransfer?.types || []).includes("Files")) event.preventDefault();
    };
    document.addEventListener("dragover", preventFileNavigation);
    document.addEventListener("drop", preventFileNavigation);
    return () => {
      document.removeEventListener("dragover", preventFileNavigation);
      document.removeEventListener("drop", preventFileNavigation);
    };
  }, []);

  useEffect(() => { depth.current = 0; setDragging(false); setError(""); }, [disabled, accept]);

  const choose = async (files: File[]) => {
    if (disabled || processing.current || !files.length) return;
    if (files.length !== 1) { setError("每次只能选择一个文件，请逐个上传。"); return; }
    const file = files[0];
    if (!acceptedFile(file, accept)) { setError(`不支持此文件类型，请选择 ${accept} 文件。`); return; }
    if (file.size < minBytes || file.size > maxBytes) {
      setError(`${minBytes > 0 ? "文件不能为空，且" : "文件"}不能超过 ${bytes(maxBytes)}。`);
      return;
    }
    setError("");
    processing.current = true;
    setReading(true);
    try {
      await onFile(file);
      setSelected(file.name);
    } catch (error) {
      setError(error instanceof Error ? error.message : "无法读取所选文件，请重试。");
    } finally {
      processing.current = false;
      setReading(false);
    }
  };

  return <div className={`file-dropzone ${dragging && !blocked ? "file-dropzone--active" : ""} ${blocked ? "file-dropzone--disabled" : ""}`} data-testid="file-dropzone"
    onDragEnter={(event) => {
      if (!Array.from(event.dataTransfer.types).includes("Files")) return;
      event.preventDefault();
      if (!blocked && ++depth.current === 1) setDragging(true);
    }}
    onDragOver={(event) => {
      if (!Array.from(event.dataTransfer.types).includes("Files")) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = blocked ? "none" : "copy";
    }}
    onDragLeave={() => { depth.current = Math.max(0, depth.current - 1); if (!depth.current) setDragging(false); }}
    onDrop={(event) => {
      event.preventDefault(); event.stopPropagation(); depth.current = 0; setDragging(false);
      if (blocked || processing.current) return;
      const items = Array.from(event.dataTransfer.items || []);
      if (items.some((item) => item.webkitGetAsEntry?.()?.isDirectory)) {
        setError("不支持拖入文件夹，请选择单个文件或压缩包。"); return;
      }
      const files = Array.from(event.dataTransfer.files);
      if (!files.length) { setError("请从本机拖入文件，不支持链接或文件夹。"); return; }
      void choose(files);
    }}>
    <input ref={ref} className="file-dropzone__input" type="file" aria-label={label} accept={accept} disabled={blocked} aria-invalid={Boolean(error)} aria-describedby={`${descriptionId}${error ? ` ${errorId}` : ""}`} onChange={(event) => {
      const files = Array.from(event.currentTarget.files || []);
      event.currentTarget.value = ""; // Allow choosing the same file again.
      void choose(files);
    }} />
    <div className="file-dropzone__prompt" id={descriptionId}>
      <Upload size={23} aria-hidden />
      <strong>{reading ? "正在读取文件…" : disabled ? "处理中，暂不能更换文件" : dragging ? "松开以选择文件" : "拖拽文件到此处，或点击选择"}</strong>
      <span>单个文件 · 最大 {bytes(maxBytes)}</span>
      {(fileName ?? selected) && <span className="file-dropzone__filename">已选择：{fileName ?? selected}</span>}
    </div>
    {error && <span id={errorId} className="file-dropzone__error" role="alert">{error}</span>}
  </div>;
}
