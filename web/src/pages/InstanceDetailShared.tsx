import { type ReactNode } from "react";
import { EmptyState, ErrorState } from "../components/ui";
import { ApiError } from "../lib/api";


export function bytesToBase64(value: Uint8Array) {
  let binary = "";
  const chunk = 0x8000;
  for (let offset = 0; offset < value.length; offset += chunk) {
    binary += String.fromCharCode(...value.subarray(offset, offset + chunk));
  }
  return btoa(binary);
}

export function wait(milliseconds: number) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

export function OptionalUnavailable({ error, title, description, action }: { error: unknown; title: string; description: string; action?: ReactNode }) {
  if (error instanceof ApiError && [404, 501].includes(error.status)) return <EmptyState title={title} description={description} action={action} />;
  return <ErrorState error={error} />;
}

export function Toggle({ checked, onChange, label, description }: { checked: boolean; onChange: (value: boolean) => void; label: string; description: string }) {
  return <label className="toggle-field"><button type="button" role="switch" aria-label={label} aria-checked={checked} className={`switch ${checked ? "switch--on" : ""}`} onClick={() => onChange(!checked)}><span /></button><span><strong>{label}</strong><small>{description}</small></span></label>;
}

export function safeBrowserFilename(value: string) {
  return value.trim().replace(/[\\/:*?"<>|\u0000-\u001f]/g, "_") || "download.bin";
}

export function saveBrowserDownload(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}
