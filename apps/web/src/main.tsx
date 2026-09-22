import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  BookOpen,
  LayoutDashboard,
  Server,
  Route,
  RefreshCw,
  Clock,
  CreditCard,
  Wallet as WalletIcon,
  Ticket,
  Settings as SettingsIcon,
  Users,
  ShieldCheck,
  ChartNoAxesCombined,
  Activity,
  LogOut,
  Menu,
  X,
  Database,
  KeyRound,
  Package,
  FileText,
  Bell,
} from "lucide-react";
import { api, post, RecordData } from "./api";
import {
  Brand,
  Header,
  Button,
  Notice,
  Loading,
  Empty,
  useData,
  ErrorNotice,
} from "./ui";
import { AuthPage } from "./auth";
import { ToolPage, TasksPage } from "./tools";
import { Account, Wallet, Plans, Orders, Routes, Traffic } from "./business";
import { ResourcePage, CodesPage, Settings, AdminRules } from "./admin";
import { Articles, Tickets } from "./content";
import { UsersPage } from "./users";
import { RouteBuilder } from "./tunnels";
import { Backups } from "./backups";
import { navigationGroups } from "./navigation";
import { paymentReturnOrder } from "./payment-return";
import "./app.css";
const labels: Record<string, [string, typeof Server]> = {
  tutorials: ["公告/教程", BookOpen],
  home: ["工作台", LayoutDashboard],
  deploy: ["部署 MSBOOST", Server],
  relay: ["配置中转服务器", Route],
  dd: ["DD 系统", RefreshCw],
  tasks: ["任务记录", Clock],
  plans: ["枫叶兑换", Package],
  wallet: ["我的枫叶", WalletIcon],
  orders: ["兑换记录", FileText],
  tickets: ["工单", Ticket],
  account: ["账户与邮箱验证", ShieldCheck],
  routes: ["中转线路", Route],
  traffic: ["流量统计", ChartNoAxesCombined],
  status: ["线路状态", Activity],
  users: ["用户管理", Users],
  agents: ["节点", Server],
  executors: ["控制执行机", Server],
  cards: ["兑换码管理", CreditCard],
  invitations: ["邀请码管理", KeyRound],
  payments: ["兑换渠道", CreditCard],
  settings: ["系统设置", SettingsIcon],
  backups: ["备份与恢复", Database],
  rules: ["用户中转", Route],
  release: ["发布维护", Package],
};
function Overview({
  admin,
  navigate,
}: {
  admin: boolean;
  navigate: (v: string) => void;
}) {
  const { data, error } = useData(admin ? "/api/admin/overview" : null);
  return (
    <>
      <Header
        title={admin ? "运营概览" : "从你的专属节点开始"}
        sub={
          admin
            ? "查看免费工具、线路和系统的运行情况。"
            : "部署、配置和再次下载，都在一个工作空间里。"
        }
      />
      <ErrorNotice error={error} />
      {admin ? (
        <div className="grid4">
          {[
            ["users", "注册用户", "人"],
            ["tasks", "任务", "次"],
            ["routes", "隧道", "条"],
            ["orders", "兑换记录", "笔"],
          ].map(([k, n, u]) => (
            <div className="card stat" key={k}>
              <div className="stat-top">{n}</div>
              <div className="stat-value">
                {data?.[k] ?? "—"}
                <span>{u}</span>
              </div>
            </div>
          ))}
        </div>
      ) : (
        <div className="welcome">
          <div>
            <h2>准备好服务器，接下来交给工具。</h2>
            <p>选择一项操作开始。完成后，请及时下载并备份配置。</p>
          </div>
          <Route size={64} className="orange" />
        </div>
      )}
      <div className="grid3 mt24">
        {["deploy", "relay", "dd"].map((k) => {
          const [n, I] = labels[k];
          return (
            <button
              key={k}
              className="card tool-card"
              onClick={() => navigate(k)}
            >
              <span className="tool-icon">
                <I size={24} />
              </span>
              <div>
                <h3>{n}</h3>
                <p>
                  {k === "deploy"
                    ? "安装节点并生成直连配置"
                    : k === "relay"
                      ? "导入 JSON，自动分配中转端口"
                      : "Debian 12 重装与参数保留"}
                </p>
              </div>
            </button>
          );
        })}
      </div>
      {admin && (
        <div className="card mt24">
          <h3>首次配置</h3>
          <p className="muted mt8">
            添加控制执行机后即可使用在线工具。添加节点、隧道和权益后可提供捐赠权益转发服务。
          </p>
          <div className="actions mt16">
            {["executors", "agents", "routes", "plans", "settings"].map((k) => (
              <Button key={k} onClick={() => navigate(k)}>
                {k === "routes" ? "隧道" : labels[k][0]}
              </Button>
            ))}
          </div>
        </div>
      )}
    </>
  );
}
function Release() {
  const { data } = useData("/api/health");
  return (
    <>
      <Header title="发布维护" />
      <div className="card">
        <h3>当前服务版本 {data?.version || "—"}</h3>
        <p className="muted mt16">
          升级前请在备份页创建并下载加密备份，同时独立保管主密钥。使用项目提供的
          Compose 部署配置和固定版本镜像升级；现有数据卷会保留。
        </p>
        <Notice>
          客户节点安装、执行
          Agent、中转运行时和网站分别发布。本站升级不会自动重装客户 VPS。
        </Notice>
      </div>
    </>
  );
}
function App() {
  const [user, setUser] = useState<RecordData | null>(null),
    [settings, setSettings] = useState<RecordData>({}),
    [loading, setLoading] = useState(true),
    [error, setError] = useState(""),
    [view, setView] = useState(
      location.hash.slice(1) || (paymentReturnOrder() ? "orders" : "tutorials"),
    ),
    [drawer, setDrawer] = useState(false);
  async function refresh() {
    try {
      const [st, me] = await Promise.all([
        api("/api/settings"),
        api("/api/me").catch((e) => {
          if (e.status === 401) return null;
          throw e;
        }),
      ]);
      setSettings(st.settings || st);
      setUser(me?.user || null);
      setError("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }
  useEffect(() => {
    void refresh();
    const h = () =>
      setView(
        location.hash.slice(1) ||
          (paymentReturnOrder() ? "orders" : "tutorials"),
      );
    addEventListener("hashchange", h);
    return () => removeEventListener("hashchange", h);
  }, []);
  useEffect(() => {
    const close = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDrawer(false);
    };
    addEventListener("keydown", close);
    return () => removeEventListener("keydown", close);
  }, []);
  function navigate(v: string) {
    location.hash = v;
    setView(v);
    setDrawer(false);
    scrollTo(0, 0);
  }
  const admin = user?.role === "admin";
  const nav = navigationGroups(admin, !!user && user.expiresAt > Date.now());
  if (loading) return <Loading />;
  if (error && !user)
    return (
      <div className="card" style={{ maxWidth: 600, margin: "80px auto" }}>
        <Brand />
        <div className="mt24">
          <Notice tone="red">无法连接 MSBOOST 服务：{error}</Notice>
        </div>
        <Button className="mt16" onClick={() => void refresh()}>
          重试
        </Button>
      </div>
    );
  if (!user)
    return (
      <AuthPage
        settings={settings}
        onLogin={() => {
          navigate(paymentReturnOrder() ? "orders" : "tutorials");
          void refresh();
        }}
      />
    );
  let page: React.ReactNode;
  switch (view) {
    case "home":
      page = <Overview admin={admin} navigate={navigate} />;
      break;
    case "tutorials":
      page = <Articles admin={admin} />;
      break;
    case "deploy":
    case "relay":
    case "dd":
      page = (
        <ToolPage
          key={view}
          kind={view}
          user={user}
          settings={settings}
          onNavigate={navigate}
        />
      );
      break;
    case "tasks":
      page = <TasksPage user={user} />;
      break;
    case "account":
      page = <Account user={user} onRefresh={() => void refresh()} />;
      break;
    case "plans":
      page = admin ? (
        <ResourcePage kind="plans" />
      ) : (
        <Plans user={user} onRefresh={() => void refresh()} />
      );
      break;
    case "wallet":
      page = <Wallet onRefresh={() => void refresh()} />;
      break;
    case "orders":
      page = (
        <Orders
          admin={admin}
          returnOrderID={paymentReturnOrder()}
          onPaid={() => void refresh()}
        />
      );
      break;
    case "tickets":
      page = <Tickets admin={admin} />;
      break;
    case "routes":
      page = admin ? <RouteBuilder /> : <Routes user={user} />;
      break;
    case "traffic":
    case "status":
      page = <Traffic status={view === "status"} />;
      break;
    case "users":
      page = admin ? <UsersPage onRefresh={() => void refresh()} /> : <Empty />;
      break;
    case "executors":
    case "agents":
    case "payments":
      page = admin ? (
        <ResourcePage key={view} kind={view} />
      ) : (
        <Empty>此页面仅管理员可访问</Empty>
      );
      break;
    case "cards":
    case "invitations":
      page = admin ? (
        <CodesPage key={view} invitations={view === "invitations"} />
      ) : (
        <Empty />
      );
      break;
    case "settings":
      page = admin ? <Settings onRefresh={() => void refresh()} /> : <Empty />;
      break;
    case "backups":
      page = admin ? <Backups /> : <Empty />;
      break;
    case "rules":
      page = admin ? <AdminRules /> : <Empty />;
      break;
    case "release":
      page = admin ? <Release /> : <Empty />;
      break;
    default:
      page = <Articles />;
  }
  const title =
    admin && view === "plans"
      ? "权益管理"
      : admin && view === "routes"
        ? "隧道"
        : admin && view === "tasks"
          ? "任务审计"
          : labels[view]?.[0] || "工作台";
  return (
    <>
      {drawer && (
        <button
          className="drawer-backdrop"
          aria-label="关闭导航遮罩"
          onClick={() => setDrawer(false)}
        />
      )}
      <aside className={"sidebar " + (drawer ? "open" : "")}>
        <Button
          className="mobile-menu drawer-close"
          aria-label="关闭导航菜单"
          onClick={() => setDrawer(false)}
        >
          <X size={18} />
        </Button>
        <Brand />
        <div className="side-account">
          <span className="avatar">{admin ? "管" : "M"}</span>
          <div>
            <b>{user.email}</b>
            <small>
              {admin
                ? "管理员"
                : user.expiresAt > Date.now()
                  ? "权益用户"
                  : "注册用户"}
            </small>
          </div>
        </div>
        <nav className="side-nav" aria-label="主导航">
          {nav.map((group) => (
            <section
              className="nav-group"
              aria-label={group.title}
              key={group.title}
            >
              <h2 className="nav-group-title">{group.title}</h2>
              {group.items.map((k) => {
                const [n, I] = labels[k];
                const name =
                  admin && k === "routes"
                    ? "隧道"
                    : admin && k === "plans"
                      ? "权益管理"
                      : admin && k === "home"
                        ? "运营概览"
                        : admin && k === "tasks"
                          ? "任务审计"
                          : admin && k === "orders"
                            ? "兑换记录管理"
                            : n;
                return (
                  <button
                    key={k}
                    className={"nav-item " + (view === k ? "active" : "")}
                    aria-current={view === k ? "page" : undefined}
                    onClick={() => navigate(k)}
                  >
                    <I size={18} />
                    {name}
                  </button>
                );
              })}
            </section>
          ))}
        </nav>
        <div className="side-footer">
          <span>MSBOOST</span>
          <button
            onClick={async () => {
              try {
                await post("/api/auth/logout", {});
                setUser(null);
              } catch (e) {
                setError((e as Error).message);
              }
            }}
          >
            <LogOut size={14} /> 退出登录
          </button>
        </div>
      </aside>
      <div className="app-shell">
        <header className="topbar">
          <div className="flex">
            <Button
              className="mobile-menu"
              aria-label="展开导航菜单"
              aria-expanded={drawer}
              onClick={() => setDrawer(!drawer)}
            >
              <Menu size={20} />
            </Button>
            <div className="crumb">
              工作空间 / <b>{title}</b>
            </div>
          </div>
          <div className="top-right">
            <TicketBell navigate={navigate} />
            <BadgeLabel user={user} />
            <button onClick={() => navigate("account")}>{user.email}</button>
          </div>
        </header>
        <main className="content">
          {!window.isSecureContext && (
            <Notice tone="orange">
              当前是明文 HTTP 调试连接。请勿填写真实 SSH
              密码、支付密钥或注册令牌；在线执行机与节点需要
              HTTPS，本机加密配置存储也需要安全连接。
            </Notice>
          )}
          {settings.maintenance && !admin && (
            <div className="mb16">
              <Notice tone="orange">
                站点正在维护，新任务暂时暂停，历史记录仍可查看。
              </Notice>
            </div>
          )}
          <ErrorNotice error={error} />
          {page}
          <footer className="footer">
            <span>© {new Date().getFullYear()} MSBOOST</span>
            <span>YOUR SERVER. YOUR ROUTE.</span>
          </footer>
        </main>
      </div>
    </>
  );
}
function TicketBell({ navigate }: { navigate: (view: string) => void }) {
  const { data, reload } = useData("/api/tickets");
  const unread = Math.max(0, Number(data?.unreadCount) || 0);
  useEffect(() => {
    const timer = window.setInterval(reload, 15000);
    window.addEventListener("msboost:tickets-changed", reload);
    return () => {
      window.clearInterval(timer);
      window.removeEventListener("msboost:tickets-changed", reload);
    };
  }, []);
  return (
    <button
      className={`ticket-bell ${unread > 0 ? "has-unread" : ""}`}
      aria-label={unread > 0 ? `工单消息，${unread}条未读` : "工单消息，无未读"}
      title={unread > 0 ? `${unread} 条未读工单消息` : "暂无未读工单消息"}
      onClick={() => navigate("tickets")}
    >
      <Bell size={19} fill={unread > 0 ? "currentColor" : "none"} />
      {unread > 0 && <span>{unread > 99 ? "99+" : unread}</span>}
    </button>
  );
}
function BadgeLabel({ user }: { user: RecordData }) {
  return (
    <span className="badge orange">
      {user.role === "admin"
        ? "管理员"
        : user.expiresAt > Date.now()
          ? "捐赠权益有效"
          : "免费工具"}
    </span>
  );
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
