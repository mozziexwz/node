import { sha256 } from "@noble/hashes/sha2.js";
export type RecordData = Record<string, any>;
let csrf = "";
export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}
export async function api<T = RecordData>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const headers = new Headers(options.headers);
  if (options.body && !(options.body instanceof FormData))
    headers.set("Content-Type", "application/json");
  if (csrf) headers.set("X-CSRF-Token", csrf);
  const response = await fetch(path, {
    ...options,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  const type = response.headers.get("Content-Type") || "";
  const result = type.includes("json")
    ? await response.json()
    : await response.text();
  if (!response.ok)
    throw new APIError(
      result.error || `请求失败 (${response.status})`,
      response.status,
    );
  if (result?.csrfToken) csrf = result.csrfToken;
  return result as T;
}
const attempts = new Map<string, { id: string; at: number }>();
export async function post<T = RecordData>(
  path: string,
  data: unknown,
  method = "POST",
  headers?: HeadersInit,
): Promise<T> {
  const h = new Headers(headers);
  let body = data;
  let fingerprint = "";
  const object = data as RecordData;
  if (object?.requestId || h.has("Idempotency-Key")) {
    const withoutID = { ...object };
    delete withoutID.requestId;
    const digest = sha256(
      new TextEncoder().encode(path + method + JSON.stringify(withoutID)),
    );
    fingerprint = Array.from(new Uint8Array(digest), (b) =>
      b.toString(16).padStart(2, "0"),
    ).join("");
    const previous = attempts.get(fingerprint);
    const id =
      previous?.id || String(object.requestId || h.get("Idempotency-Key"));
    if (!previous && attempts.size >= 100)
      throw new Error("待核对的请求过多，请先查看任务或订单记录。");
    attempts.set(fingerprint, { id, at: Date.now() });
    if (object.requestId) body = { ...object, requestId: id };
    if (h.has("Idempotency-Key")) h.set("Idempotency-Key", id);
  }
  const result = await api<T>(path, {
    method,
    body: JSON.stringify(body),
    headers: h,
  });
  if (fingerprint) attempts.delete(fingerprint);
  return result;
}
export function array(data: any, key: string): RecordData[] {
  return Array.isArray(data)
    ? data
    : Array.isArray(data?.[key])
      ? data[key]
      : [];
}
export const requestID = () => {
  if (crypto.randomUUID) return crypto.randomUUID();
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64;
  bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
    "",
  );
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
};
export async function copyText(value: string) {
  if (navigator.clipboard?.writeText)
    return navigator.clipboard.writeText(value);
  const input = document.createElement("textarea");
  input.value = value;
  input.style.position = "fixed";
  input.style.opacity = "0";
  (document.querySelector("dialog[open]") || document.body).appendChild(input);
  input.select();
  try {
    if (!document.execCommand("copy"))
      throw new Error("无法自动复制，请手动选择并复制文本");
  } finally {
    input.remove();
  }
}
export function download(data: unknown, name: string) {
  const url = URL.createObjectURL(
    new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }),
  );
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export async function downloadFile(path: string, name: string) {
  const response = await fetch(path, {
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) {
    const body = await response.json();
    throw new Error(body.error || "下载失败");
  }
  const url = URL.createObjectURL(await response.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
