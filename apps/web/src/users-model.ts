import type { RecordData } from "./api";

export const DAY_MS = 86_400_000;
export type UserSortKey =
  "balanceCents" | "days" | "trafficTotal" | "trafficUsed" | "rateMbps";
export type UserDraft = {
  email: string;
  password: string;
  role: string;
  status: string;
  balance: string;
  days: string;
  total: string;
  used: string;
  rate: string;
  reason: string;
};

// Convert decimal input with integer arithmetic. A partial GB never acquires
// floating-point rounding bytes; the UI accepts枫叶 only as whole units.
export function unitInteger(
  input: string,
  decimals: number,
  label: string,
): number {
  const text = input.trim();
  if (!/^\d+(?:\.\d+)?$/.test(text)) throw new Error(`${label}需为非负数字`);
  const [whole, fraction = ""] = text.split(".");
  if (fraction.length > decimals)
    throw new Error(`${label}最多保留 ${decimals} 位小数`);
  const value =
    BigInt(whole) * 10n ** BigInt(decimals) +
    BigInt(fraction.padEnd(decimals, "0") || "0");
  if (value > BigInt(Number.MAX_SAFE_INTEGER))
    throw new Error(`${label}超出可安全保存的范围`);
  return Number(value);
}

export function integerUnit(value: number, decimals: number): string {
  if (!Number.isSafeInteger(value) || value < 0) return "";
  const raw = String(value).padStart(decimals + 1, "0");
  return decimals
    ? `${raw.slice(0, -decimals)}.${raw.slice(-decimals)}`.replace(
        /\.?0+$/,
        "",
      ) || "0"
    : raw;
}

export function remainingDays(user: RecordData, now = Date.now()): number {
  return Math.max(0, (Number(user.expiresAt || 0) - now) / DAY_MS);
}

export function userDraft(
  user: RecordData | null,
  now = Date.now(),
): UserDraft {
  return {
    email: user?.email || "",
    password: "",
    role: user?.role === "admin" ? "admin" : "member",
    status: user?.status || "active",
    balance: integerUnit(Math.trunc(Number(user?.balanceCents || 0) / 100), 0),
    days: user ? remainingDays(user, now).toFixed(2) : "0",
    total: integerUnit(Number(user?.trafficTotal || 0), 9),
    used: integerUnit(Number(user?.trafficUsed || 0), 9),
    rate: String(user?.rateMbps || 1),
    reason: "管理员调整",
  };
}

export function userPatch(
  original: RecordData | null,
  baseline: UserDraft,
  draft: UserDraft,
  now = Date.now(),
): RecordData {
  const patch: RecordData = {};
  for (const key of ["email", "role", "status"] as const)
    if (!original || draft[key] !== baseline[key])
      patch[key] = draft[key].trim();
  if (draft.password) patch.password = draft.password;
  if (!original && !draft.password) throw new Error("新增用户必须设置密码");
  if (!original || draft.balance !== baseline.balance) {
    const balance = unitInteger(draft.balance, 0, "枫叶");
    if (balance > Math.floor(Number.MAX_SAFE_INTEGER / 100))
      throw new Error("枫叶超出可安全保存的范围");
    patch.balanceCents = balance * 100;
  }
  for (const [field, key, decimals, label] of [
    ["total", "trafficTotal", 9, "总流量（GB）"],
    ["used", "trafficUsed", 9, "已用流量（GB）"],
    ["rate", "rateMbps", 0, "速率（Mbps）"],
  ] as const) {
    if (!original || draft[field] !== baseline[field])
      patch[key] = unitInteger(draft[field], decimals, label);
  }
  // Crucially, do not send a recalculated deadline when only identity or other
  // fields changed. The displayed rounded day count is not the stored deadline.
  if (!original || draft.days !== baseline.days) {
    const microDays = unitInteger(draft.days, 6, "剩余天数");
    const duration =
      (BigInt(microDays) * BigInt(DAY_MS) + 500_000n) / 1_000_000n;
    const expiry = microDays === 0 ? 0n : BigInt(now) + duration;
    if (expiry > BigInt(Number.MAX_SAFE_INTEGER))
      throw new Error("剩余天数超出可安全保存的范围");
    patch.expiresAt = Number(expiry);
  }
  if ((patch.rateMbps ?? original?.rateMbps ?? 1) < 1)
    throw new Error("速率至少为 1 Mbps");
  if (
    (patch.trafficUsed ?? original?.trafficUsed ?? 0) >
    (patch.trafficTotal ?? original?.trafficTotal ?? 0)
  )
    throw new Error("已用流量不能超过总流量");
  if (
    patch.balanceCents !== undefined &&
    patch.balanceCents !== (original?.balanceCents ?? 0) &&
    !draft.reason.trim()
  )
    throw new Error("调整枫叶请填写原因");
  if (draft.reason.trim()) patch.reason = draft.reason.trim();
  return patch;
}

export function sortedUsers(
  users: RecordData[],
  key: UserSortKey,
  direction: "asc" | "desc",
  now = Date.now(),
): RecordData[] {
  const value = (u: RecordData) =>
    key === "days" ? remainingDays(u, now) : Number(u[key] || 0);
  return [...users].sort((a, b) => {
    const diff = value(a) - value(b);
    if (diff) return direction === "asc" ? diff : -diff;
    return String(a.email).localeCompare(String(b.email), "zh-CN");
  });
}
