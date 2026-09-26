import { useState } from "react";
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Pencil,
  Plus,
  KeyRound,
  Trash2,
} from "lucide-react";
import { api, array, post, type RecordData } from "./api";
import { validateNewPassword } from "./auth-code";
import {
  Header,
  Button,
  Field,
  Select,
  Notice,
  Modal,
  Badge,
  ErrorNotice,
  AsyncForm,
  useData,
  date,
  gb,
  leaves,
} from "./ui";
import {
  remainingDays,
  sortedUsers,
  userDraft,
  userPatch,
  type UserDraft,
  type UserSortKey,
} from "./users-model";
import "./users.css";

export function UsersPage({ onRefresh }: { onRefresh?: () => void } = {}) {
  const { data, error, reload } = useData("/api/admin/users");
  const [search, setSearch] = useState(""),
    [message, setMessage] = useState("");
  const [sort, setSort] = useState<{
    key: UserSortKey;
    direction: "asc" | "desc";
  }>({ key: "balanceCents", direction: "desc" });
  const [editing, setEditing] = useState<{
    user: RecordData | null;
    baseline: UserDraft;
    draft: UserDraft;
  } | null>(null);
  const [reset, setReset] = useState<RecordData | null>(null);
  const now = Date.now(),
    users = sortedUsers(
      array(data, "users").filter((u) =>
        String(u.email).toLowerCase().includes(search.trim().toLowerCase()),
      ),
      sort.key,
      sort.direction,
      now,
    );
  function open(user: RecordData | null) {
    const draft = userDraft(user);
    setEditing({ user, baseline: { ...draft }, draft });
    setMessage("");
  }
  function change(key: keyof UserDraft, value: string) {
    setEditing((current) =>
      current
        ? { ...current, draft: { ...current.draft, [key]: value } }
        : current,
    );
  }
  function refresh() {
    reload();
    onRefresh?.();
  }
  async function action(work: () => Promise<unknown>) {
    try {
      setMessage("");
      await work();
      refresh();
    } catch (e) {
      setMessage((e as Error).message);
    }
  }
  function sorting(key: UserSortKey, label: string) {
    const selected = sort.key === key;
    const Icon = selected
      ? sort.direction === "asc"
        ? ArrowUp
        : ArrowDown
      : ArrowUpDown;
    return (
      <th
        aria-sort={
          selected
            ? sort.direction === "asc"
              ? "ascending"
              : "descending"
            : "none"
        }
      >
        <button
          type="button"
          className="user-sort"
          onClick={() =>
            setSort({
              key,
              direction: selected && sort.direction === "desc" ? "asc" : "desc",
            })
          }
        >
          {label}
          <Icon size={14} />
        </button>
      </th>
    );
  }
  return (
    <>
      <Header title="用户管理" sub="管理账号、邮箱验证、枫叶与权益。">
        <Button primary onClick={() => open(null)}>
          <Plus size={16} />
          添加用户
        </Button>
      </Header>
      <ErrorNotice error={error || message} />
      <div className="card user-filter">
        <Field
          label="搜索用户"
          placeholder="输入 QQ 邮箱"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <span className="muted">{users.length} 位用户 · 点击表头可排序</span>
      </div>
      <div className="card flush mt16">
        <div className="table-wrap">
          <table className="users-table">
            <thead>
              <tr>
                <th>用户 / 注册时间</th>
                <th>邮箱验证</th>
                {sorting("balanceCents", "枫叶")}
                {sorting("days", "剩余天数")}
                <th>权益等级</th>
                {sorting("trafficTotal", "总流量（GB）")}
                {sorting("trafficUsed", "已用流量（GB）")}
                {sorting("rateMbps", "规则速率（Mbps）")}
                <th>状态</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>
                    <strong>{u.email}</strong>
                    <small>{date(u.createdAt)}</small>
                    {u.role === "admin" && <Badge>管理员</Badge>}
                  </td>
                  <td>
                    <Badge tone={u.emailVerifiedAt ? "green" : "orange"}>
                      {u.emailVerifiedAt ? "已验证" : "未验证"}
                    </Badge>
                    {!!u.emailVerifiedAt && (
                      <small>{date(u.emailVerifiedAt)}</small>
                    )}
                  </td>
                  <td>{leaves(u.balanceCents)}枫叶</td>
                  <td
                    title={
                      u.expiresAt ? `到期：${date(u.expiresAt)}` : "尚未开通"
                    }
                  >
                    {remainingDays(u, now).toFixed(2)} 天
                  </td>
                  <td>{`L${Math.min(3, Math.max(1, Number(u.level) || 1))}`}</td>
                  <td>{gb(u.trafficTotal)} GB</td>
                  <td>{gb(u.trafficUsed)} GB</td>
                  <td>{u.rateMbps || 1} Mbps</td>
                  <td>
                    <Badge tone={u.status === "active" ? "green" : "red"}>
                      {u.status === "active" ? "正常" : "已暂停"}
                    </Badge>
                  </td>
                  <td>
                    <div className="actions user-actions">
                      <Button className="small" onClick={() => open(u)}>
                        <Pencil size={14} />
                        编辑
                      </Button>
                      <Button
                        className="small"
                        onClick={() =>
                          void action(() =>
                            post(
                              `/api/admin/users/${u.id}`,
                              {
                                status:
                                  u.status === "active"
                                    ? "suspended"
                                    : "active",
                              },
                              "PATCH",
                            ),
                          )
                        }
                      >
                        {u.status === "active" ? "暂停" : "恢复"}
                      </Button>
                      <Button className="small" onClick={() => setReset(u)}>
                        <KeyRound size={14} />
                        重置密码
                      </Button>
                      <Button
                        className="small danger"
                        onClick={() => {
                          if (
                            confirm(
                              `删除用户 ${u.email}？账号与登录会话将移除，审计和财务记录保留。`,
                            )
                          )
                            void action(() =>
                              api(`/api/admin/users/${u.id}`, {
                                method: "DELETE",
                              }),
                            );
                        }}
                      >
                        <Trash2 size={14} />
                        删除
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
              {!users.length && (
                <tr>
                  <td colSpan={10}>
                    <div className="empty">没有符合条件的用户</div>
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
      {editing && (
        <Modal
          title={editing.user ? `编辑用户 · ${editing.user.email}` : "添加用户"}
          onClose={() => setEditing(null)}
        >
          <AsyncForm
            onSubmit={async () => {
              if (editing.draft.password)
                validateNewPassword(
                  editing.draft.password,
                  editing.draft.password,
                  editing.draft.email,
                );
              const patch = userPatch(
                editing.user,
                editing.baseline,
                editing.draft,
              );
              await post(
                "/api/admin/users" +
                  (editing.user ? "/" + editing.user.id : ""),
                patch,
                editing.user ? "PATCH" : "POST",
              );
              setEditing(null);
              refresh();
            }}
          >
            <div className="form-grid">
              <Field
                label="QQ 邮箱"
                type="email"
                value={editing.draft.email}
                onChange={(e) => change("email", e.target.value)}
                required
              />
              <Field
                label={editing.user ? "新密码（留空不修改）" : "密码"}
                type="password"
                value={editing.draft.password}
                onChange={(e) => change("password", e.target.value)}
                required={!editing.user}
                minLength={8}
                autoComplete="new-password"
              />
              <Select
                label="角色"
                value={editing.draft.role}
                onChange={(e) => change("role", e.target.value)}
              >
                <option value="member">普通用户</option>
                <option value="admin">管理员</option>
              </Select>
              <Select
                label="状态"
                value={editing.draft.status}
                onChange={(e) => change("status", e.target.value)}
              >
                <option value="active">正常</option>
                <option value="suspended">已暂停</option>
              </Select>
            </div>
            {editing.user && (
              <div className="user-verification mt16">
                <span>当前邮箱</span>
                <Badge tone={editing.user.emailVerifiedAt ? "green" : "orange"}>
                  {editing.user.emailVerifiedAt ? "已验证" : "未验证"}
                </Badge>
                <small>
                  {editing.user.emailVerifiedAt
                    ? date(editing.user.emailVerifiedAt)
                    : "修改邮箱后需要重新验证"}
                </small>
              </div>
            )}
            <div className="form-grid mt16">
              <Select
                label="权益等级"
                value={editing.draft.level}
                onChange={(e) => change("level", e.target.value)}
              >
                <option value="1">L1</option>
                <option value="2">L2</option>
                <option value="3">L3</option>
              </Select>
              <Field
                label="枫叶"
                type="number"
                min="0"
                step="1"
                value={editing.draft.balance}
                onChange={(e) => change("balance", e.target.value)}
                required
              />
              <Field
                label="权益剩余天数"
                type="number"
                min="0"
                step="any"
                value={editing.draft.days}
                onChange={(e) => change("days", e.target.value)}
                hint={
                  editing.user?.expiresAt
                    ? `原到期时间：${date(editing.user.expiresAt)}；仅修改此项才重设到期时间。`
                    : "0 表示未开通；填写天数后从保存时开始计算。"
                }
                required
              />
              <Field
                label="总流量（GB）"
                type="number"
                min="0"
                step="any"
                value={editing.draft.total}
                onChange={(e) => change("total", e.target.value)}
                required
              />
              <Field
                label="已用流量（GB）"
                type="number"
                min="0"
                step="any"
                value={editing.draft.used}
                onChange={(e) => change("used", e.target.value)}
                hint="设为 0 保存时，将同步重置该用户所有线路的已用上行、下行流量计数。"
                required
              />
              <Field
                label="每条规则速率（Mbps）"
                type="number"
                min="1"
                max="1000000"
                step="1"
                value={editing.draft.rate}
                onChange={(e) => change("rate", e.target.value)}
                required
              />
              <Field
                label="调整原因"
                value={editing.draft.reason}
                onChange={(e) => change("reason", e.target.value)}
                hint="调整枫叶时必填，会写入枫叶明细。"
              />
            </div>
            <Notice>
              枫叶以整数、流量以十进制 GB 输入。手动添加用户不会自动验证邮箱。
            </Notice>
          </AsyncForm>
        </Modal>
      )}
      {reset && (
        <Modal
          title={`重置密码 · ${reset.email}`}
          onClose={() => setReset(null)}
        >
          <AsyncForm
            label="重置密码"
            onSubmit={async (f) => {
              const password = String(f.get("password"));
              validateNewPassword(password, password, reset.email);
              await post(`/api/admin/users/${reset.id}/password`, {
                password,
                reason: f.get("reason"),
              });
              setReset(null);
              refresh();
            }}
          >
            <Field
              label="新密码"
              name="password"
              type="password"
              minLength={8}
              required
              autoComplete="new-password"
            />
            <Field label="原因" name="reason" defaultValue="管理员重置密码" />
            <Notice>保存后该用户的现有登录会话将失效。</Notice>
          </AsyncForm>
        </Modal>
      )}
    </>
  );
}
