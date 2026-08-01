import { AlertTriangle, Inbox, LoaderCircle, X } from "lucide-react";
import { useEffect, useId, useRef, type ButtonHTMLAttributes, type HTMLAttributes, type ReactNode } from "react";
import { statusLabels } from "../lib/format";

export function Button({ className = "", variant = "primary", loading, children, type = "button", ...props }: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "danger" | "ghost";
  loading?: boolean;
}) {
  return (
    <button {...props} type={type} className={`button button--${variant} ${className}`} disabled={loading || props.disabled}>
      {loading && <LoaderCircle className="spin" size={16} aria-hidden />}
      {children}
    </button>
  );
}

export function Card({ className = "", ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={`card ${className}`} {...props} />;
}

export function StatusBadge({ status, pulse = false }: { status: string; pulse?: boolean }) {
  return (
    <span className={`status status--${status}`}>
      <span className={`status__dot ${pulse ? "pulse" : ""}`} aria-hidden />
      {statusLabels[status] || status}
    </span>
  );
}

export function PageHeader({ eyebrow, title, description, actions }: {
  eyebrow?: string;
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <header className="page-header">
      <div>
        {eyebrow && <div className="eyebrow">{eyebrow}</div>}
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      {actions && <div className="page-header__actions">{actions}</div>}
    </header>
  );
}

export function EmptyState({ title, description, action }: { title: string; description: string; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <div className="empty-state__icon"><Inbox size={24} /></div>
      <h3>{title}</h3>
      <p>{description}</p>
      {action}
    </div>
  );
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const message = error instanceof Error ? error.message : "数据加载失败";
  return (
    <div className="error-state" role="alert">
      <AlertTriangle size={20} />
      <div><strong>暂时无法获取数据</strong><span>{message}</span></div>
      {retry && <Button variant="secondary" onClick={retry}>重试</Button>}
    </div>
  );
}

export function LoadingBlock({ label = "正在加载" }: { label?: string }) {
  return <div className="loading-block" role="status"><LoaderCircle className="spin" size={22} />{label}</div>;
}

export function ProgressBar({ value, tone = "green" }: { value: number; tone?: "green" | "orange" | "red" }) {
  const safe = Math.min(100, Math.max(0, value || 0));
  return <div className={`progress progress--${tone}`} aria-label={`进度 ${safe}%`}><span style={{ width: `${safe}%` }} /></div>;
}

export function Modal({ open, title, description, children, onClose, footer }: {
  open: boolean;
  title: string;
  description?: string;
  children: ReactNode;
  onClose: () => void;
  footer?: ReactNode;
}) {
  const dialog = useRef<HTMLElement>(null);
  const closeRef = useRef(onClose);
  const titleId = useId();
  const descriptionId = useId();
  useEffect(() => { closeRef.current = onClose; }, [onClose]);
  useEffect(() => {
    if (!open) return;
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const focusableSelector = "button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [href], [tabindex]:not([tabindex='-1'])";
    const focusDialog = window.requestAnimationFrame(() => {
      if (dialog.current?.contains(document.activeElement)) return;
      (dialog.current?.querySelector<HTMLElement>("[autofocus]") || dialog.current?.querySelector<HTMLElement>(focusableSelector) || dialog.current)?.focus();
    });
    const handleKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeRef.current();
        return;
      }
      if (event.key !== "Tab" || !dialog.current) return;
      const focusable = Array.from(dialog.current.querySelectorAll<HTMLElement>(focusableSelector)).filter((element) => element.offsetParent !== null || element === document.activeElement);
      if (!focusable.length) {
        event.preventDefault();
        dialog.current.focus();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", handleKey);
    return () => {
      window.cancelAnimationFrame(focusDialog);
      document.removeEventListener("keydown", handleKey);
      document.body.style.overflow = previousOverflow;
      if (previousFocus?.isConnected) previousFocus.focus();
    };
  }, [open]);
  if (!open) return null;
  return (
    <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <section ref={dialog} className="modal" role="dialog" aria-modal="true" aria-labelledby={titleId} aria-describedby={description ? descriptionId : undefined} tabIndex={-1}>
        <header className="modal__header">
          <div><h2 id={titleId}>{title}</h2>{description && <p id={descriptionId}>{description}</p>}</div>
          <button className="icon-button" onClick={onClose} aria-label="关闭"><X size={20} /></button>
        </header>
        <div className="modal__body">{children}</div>
        {footer && <footer className="modal__footer">{footer}</footer>}
      </section>
    </div>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return <label className="field"><span>{label}</span>{children}{hint && <small>{hint}</small>}</label>;
}

export function MetricBar({ label, value, detail }: { label: string; value: number; detail: string }) {
  const tone = value >= 95 ? "red" : value >= 85 ? "orange" : "green";
  return <div className="metric-bar"><div><span>{label}</span><strong>{detail}</strong></div><ProgressBar value={value} tone={tone} /></div>;
}
