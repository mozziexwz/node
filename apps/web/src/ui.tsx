import React, { useEffect, useRef, useState } from "react";
import { X, Info, LoaderCircle } from "lucide-react";
import DOMPurify from "dompurify";
import { marked } from "marked";
import { api, RecordData } from "./api";
export const money = (n: number = 0) => (n / 100).toFixed(2);
export const date = (n: number | string) =>
  n ? new Date(n).toLocaleString("zh-CN", { hour12: false }) : "—";
export const gb = (n: number = 0) => (n / 1e9).toFixed(2);
export function Maple() {
  return (
    <svg
      viewBox="0 0 64 64"
      fill="none"
      width="24"
      height="26"
      aria-hidden="true"
    >
      <path
        d="M5 31a27 27 0 0 0 54 0"
        stroke="#000000"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
      <path
        d="M204 24C197 49 182 83 168 78C160 98 157 124 132 118C142 144 154 190 141 198C126 204 95 187 79 174C66 189 44 197 21 193C38 213 49 230 46 254C64 254 94 262 101 277C111 293 88 317 80 330C105 327 129 336 146 346C165 344 191 328 201 318C201 339 199 360 191 378C186 391 200 393 204 384C211 364 213 339 210 319C225 332 245 341 264 346C284 335 309 329 330 330C315 312 303 294 307 279C312 262 338 254 361 253C358 234 369 210 384 193C363 194 342 187 331 174C308 187 276 205 265 198C253 191 268 143 276 119C256 126 245 96 241 78C225 85 211 49 204 24Z"
        transform="translate(9 3) scale(.115)"
        fill="#ffffff"
        stroke="#000000"
        strokeWidth="17.4"
        strokeLinejoin="round"
      />
    </svg>
  );
}
export function Brand() {
  return (
    <div className="brand">
      <Maple />
      <div>
        MSBOOST<small>YOUR SERVER. YOUR ROUTE.</small>
      </div>
    </div>
  );
}
export function Button({
  children,
  primary = false,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { primary?: boolean }) {
  return (
    <button
      {...props}
      className={`btn ${primary ? "primary" : ""} ${props.className || ""}`}
      type={props.type || "button"}
    >
      {children}
    </button>
  );
}
export function Notice({
  children,
  tone = "",
}: {
  children: React.ReactNode;
  tone?: string;
}) {
  return (
    <div
      className={`notice ${tone}`}
      role={tone === "red" ? "alert" : undefined}
    >
      <Info size={17} />
      <div>{children}</div>
    </div>
  );
}
export function Field({
  label,
  hint,
  ...props
}: React.InputHTMLAttributes<HTMLInputElement> & {
  label: string;
  hint?: string;
}) {
  return (
    <label className="field">
      <span>{label}</span>
      <input {...props} />
      {hint && <small>{hint}</small>}
    </label>
  );
}
export function Select({
  label,
  children,
  ...props
}: React.SelectHTMLAttributes<HTMLSelectElement> & { label: string }) {
  return (
    <label className="field">
      <span>{label}</span>
      <select {...props}>{children}</select>
    </label>
  );
}
export function Check({
  label,
  ...props
}: React.InputHTMLAttributes<HTMLInputElement> & { label: React.ReactNode }) {
  return (
    <label className="check">
      <input type="checkbox" {...props} />
      <span>{label}</span>
    </label>
  );
}
export function Header({
  title,
  sub,
  children,
}: {
  title: string;
  sub?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="page-heading">
      <div>
        <div className="eyebrow">MSBOOST WORKSPACE</div>
        <h1>{title}</h1>
        {sub && <p>{sub}</p>}
      </div>
      {children}
    </div>
  );
}
export function Empty({
  children = "暂无记录",
}: {
  children?: React.ReactNode;
}) {
  return (
    <div className="empty">
      <Info size={26} />
      <p>{children}</p>
    </div>
  );
}
export function Badge({
  children,
  tone = "",
}: {
  children: React.ReactNode;
  tone?: string;
}) {
  return <span className={`badge ${tone}`}>{children}</span>;
}
export function Table({
  headers,
  rows,
}: {
  headers: React.ReactNode[];
  rows: React.ReactNode[][];
}) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {headers.map((h, i) => (
              <th key={i}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((cells, i) => (
            <tr key={i}>
              {cells.map((c, j) => (
                <td key={j}>{c}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {!rows.length && <Empty />}
    </div>
  );
}
export function Markdown({ text }: { text: string }) {
  return (
    <div
      className="markdown"
      dangerouslySetInnerHTML={{
        __html: DOMPurify.sanitize(
          marked.parse(text || "", { async: false }) as string,
          {
            FORBID_TAGS: ["style", "iframe", "form", "input", "svg"],
            FORBID_ATTR: ["style"],
            ADD_ATTR: ["rel"],
          },
        ),
      }}
    />
  );
}
export function Modal({
  title,
  children,
  onClose,
}: {
  title: string;
  children: React.ReactNode;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement;
    ref.current?.showModal();
    return () => previous?.focus();
  }, []);
  return (
    <dialog
      ref={ref}
      className="production-modal"
      onCancel={onClose}
      onClick={(e) => {
        if (e.target === ref.current) onClose();
      }}
    >
      <div className="between modal-heading">
        <h2>{title}</h2>
        <Button aria-label="关闭窗口" onClick={onClose}>
          <X size={18} />
        </Button>
      </div>
      {children}
    </dialog>
  );
}
export function useData<T = RecordData>(url: string | null) {
  const [data, setData] = useState<T | null>(null),
    [error, setError] = useState(""),
    [loading, setLoading] = useState(true),
    [revision, setRevision] = useState(0);
  useEffect(() => {
    let current = true;
    if (!url) {
      setLoading(false);
      return;
    }
    setLoading(true);
    api<T>(url)
      .then((d) => {
        if (current) {
          setData(d);
          setError("");
        }
      })
      .catch((e) => {
        if (current) setError(e.message);
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [url, revision]);
  return {
    data,
    error,
    loading,
    reload: () => setRevision((x) => x + 1),
    setData,
  };
}
export function Loading() {
  return (
    <div className="empty" role="status">
      <LoaderCircle className="spin" size={24} />
      正在读取…
    </div>
  );
}
export function AsyncForm({
  onSubmit,
  children,
  label = "保存",
  onDone,
}: {
  onSubmit: (f: FormData) => Promise<unknown>;
  children: React.ReactNode;
  label?: string;
  onDone?: () => void;
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [success, setSuccess] = useState(false);
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setError("");
        setSuccess(false);
        const f = new FormData(e.currentTarget);
        try {
          await onSubmit(f);
          setSuccess(true);
          onDone?.();
        } catch (e) {
          setError((e as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      {children}
      {error && (
        <div className="mt16">
          <Notice tone="red">{error}</Notice>
        </div>
      )}
      {success && (
        <div className="mt16">
          <Notice tone="green">操作已完成</Notice>
        </div>
      )}
      <div className="form-actions">
        <Button type="submit" primary disabled={busy}>
          {busy ? "正在处理…" : label}
        </Button>
      </div>
    </form>
  );
}
export function ErrorNotice({ error }: { error: string }) {
  return error ? <Notice tone="red">{error}</Notice> : null;
}
