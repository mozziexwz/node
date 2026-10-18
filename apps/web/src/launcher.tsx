import React, { useEffect, useState } from "react";
import {
  Calendar,
  Clock,
  Copy,
  Download,
  ExternalLink,
  Eye,
  FolderArchive,
  Monitor,
  MonitorDown,
  Route,
  Save,
  Server,
  Settings as SettingsIcon,
  ShieldAlert,
  ShieldCheck,
} from "lucide-react";
import { copyText, post } from "./api";
import {
  Badge,
  Button,
  ErrorNotice,
  Field,
  Header,
  Markdown,
  Notice,
  date,
  useData,
} from "./ui";

// Public launcher download, shared by the homepage and the console page. An
// empty downloadUrl means the administrator has not published it yet.
export type Launcher = {
  version?: string;
  releasedAt?: string;
  downloadUrl?: string;
  backupUrl?: string;
  sha256?: string;
  notes?: string;
  updatedAt?: number;
};

export function launcherHost(href = "") {
  try {
    return new URL(href).host;
  } catch {
    return "";
  }
}

// The settings page opens on the tab named here once, for links that should
// land on the launcher settings instead of the first tab.
export const settingsTabKey = "msboost-settings-tab";

function CopyHash({ value }: { value: string }) {
  const [done, setDone] = useState(false);
  return (
    <Button
      className="small"
      onClick={async () => {
        try {
          await copyText(value);
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } catch {
          setDone(false);
        }
      }}
    >
      <Copy size={14} />
      {done ? "已复制" : "复制"}
    </Button>
  );
}

export function LauncherDownload({
  launcher,
  heading = "h2",
  admin = false,
  onSettings,
}: {
  launcher?: Launcher;
  heading?: "h2" | "h3";
  admin?: boolean;
  onSettings?: () => void;
}) {
  if (!launcher?.downloadUrl)
    return (
      <div className="card">
        <div className="launcher-unpublished">
          <span className="launcher-mark">
            <MonitorDown size={30} />
          </span>
          <div>
            <h3>启动器下载地址尚未发布</h3>
            <p>
              {admin
                ? "到「系统设置 → MSBOOST 启动器」填写版本和下载地址后，这里会显示下载按钮。"
                : "发布后，这里会显示当前版本和下载按钮。"}
            </p>
          </div>
        </div>
        {admin && onSettings && (
          <div className="form-actions">
            <Button primary onClick={onSettings}>
              <SettingsIcon size={15} />
              去系统设置填写
            </Button>
          </div>
        )}
      </div>
    );
  const Title = heading;
  const host = launcherHost(launcher.downloadUrl);
  return (
    <div className="card">
      <div className="launcher-hero">
        <div className="launcher-id">
          <span className="launcher-mark">
            <MonitorDown size={30} />
          </span>
          <div>
            <div className="launcher-title">
              <Title>MSBOOST 启动器</Title>
              <Badge tone="orange">{launcher.version}</Badge>
              <Badge tone="green">当前版本</Badge>
            </div>
            <div className="launcher-meta">
              <span>
                <Calendar size={14} />
                更新于 {launcher.releasedAt}
              </span>
              <span>
                <Monitor size={14} />
                64 位 Windows 10 / 11
              </span>
              <span>
                <FolderArchive size={14} />
                免安装 zip，解压即用
              </span>
            </div>
          </div>
        </div>
        <div className="launcher-actions">
          <div className="row">
            <a
              className="btn primary big"
              href={launcher.downloadUrl}
              target="_blank"
              rel="noopener noreferrer"
            >
              <Download size={17} />
              下载 {launcher.version}
            </a>
            {launcher.backupUrl && (
              <a
                className="btn big"
                href={launcher.backupUrl}
                target="_blank"
                rel="noopener noreferrer"
              >
                <ExternalLink size={16} />
                备用下载
              </a>
            )}
          </div>
          {host && <small>将在新窗口打开 {host}</small>}
        </div>
      </div>
      {launcher.sha256 && (
        <div className="hash-row">
          <span>SHA-256</span>
          <code>{launcher.sha256}</code>
          <CopyHash value={launcher.sha256} />
        </div>
      )}
    </div>
  );
}

export function LauncherSteps() {
  const steps: [string, React.ReactNode][] = [
    [
      "下载并解压",
      <>
        下载 zip，解压到一个新文件夹，例如 <code>D:\MSBOOST\</code>。
      </>,
    ],
    [
      "以管理员身份运行",
      <>
        双击 <code>MSBOOST-Launcher.exe</code>，在权限提示中点「是」。
      </>,
    ],
    [
      "导入配置并开始加速",
      "在「节点配置」页导入网站下载的配置文件（.json），回到「加速」页点「开始加速」，再打开 MapleStory。",
    ],
  ];
  return (
    <div className="launcher-steps">
      {steps.map(([title, body], index) => (
        <div className="launcher-step" key={title}>
          <span className="step-num">{index + 1}</span>
          <h4>{title}</h4>
          <p>{body}</p>
        </div>
      ))}
    </div>
  );
}

export function LauncherTips() {
  return (
    <ul className="tip-list">
      <li>
        <ShieldAlert size={16} />
        <span>
          <b>SmartScreen</b>
          ：程序没有代码签名，Windows 可能提示「Windows
          已保护你的电脑」。点「更多信息」，再点「仍要运行」。
        </span>
      </li>
      <li>
        <ShieldCheck size={16} />
        <span>
          <b>杀毒软件误报</b>
          ：程序带有网络分流驱动并需要管理员权限，部分杀毒软件可能误报，可以把程序文件夹加入白名单。
        </span>
      </li>
    </ul>
  );
}

export function LauncherNotes({ launcher }: { launcher?: Launcher }) {
  if (!launcher?.downloadUrl || !launcher.notes) return null;
  return (
    <div className="launcher-notes">
      <Markdown text={launcher.notes} />
    </div>
  );
}

export function LauncherPage({
  admin,
  onNavigate,
}: {
  admin: boolean;
  onNavigate: (view: string) => void;
}) {
  const { data, error, loading } = useData("/api/public/launcher");
  const launcher: Launcher | undefined = data?.launcher;
  const hasNotes = !!launcher?.downloadUrl && !!launcher.notes;
  return (
    <>
      <Header
        title="MSBOOST 启动器"
        sub="导入部署或中转得到的配置文件，一键加速 MapleStory。只代理游戏相关进程，其他软件照常直连。"
      />
      <ErrorNotice error={error} />
      {loading && !data ? (
        <Notice>正在读取启动器信息…</Notice>
      ) : (
        <LauncherDownload
          launcher={launcher}
          admin={admin}
          onSettings={() => {
            try {
              sessionStorage.setItem(settingsTabKey, "launcher");
            } catch {
              // The settings page then opens on its first tab.
            }
            onNavigate("settings");
          }}
        />
      )}
      <div className="card mt24">
        <div className="launcher-head">
          <h3>三步开始使用</h3>
        </div>
        <LauncherSteps />
        <div className="launcher-jump">
          <span>还没有配置文件？</span>
          <Button className="small" onClick={() => onNavigate("deploy")}>
            <Server size={14} />
            去部署 MSBOOST
          </Button>
          <Button className="small" onClick={() => onNavigate("relay")}>
            <Route size={14} />
            去配置中转服务器
          </Button>
          <Button className="small" onClick={() => onNavigate("tasks")}>
            <Clock size={14} />
            在任务记录重新下载配置
          </Button>
        </div>
      </div>
      <div className={`mt24 ${hasNotes ? "launcher-grid" : ""}`}>
        {hasNotes && (
          <div className="card">
            <div className="launcher-head">
              <h3>更新内容 · {launcher?.version}</h3>
            </div>
            <LauncherNotes launcher={launcher} />
          </div>
        )}
        <div className="card">
          <div className="launcher-head">
            <h3>安全提示</h3>
          </div>
          <LauncherTips />
        </div>
      </div>
    </>
  );
}

const https = (value = "") => /^https:\/\/\S+$/.test(value.trim());

function LinkField({
  label,
  hint,
  value,
  onChange,
}: {
  label: string;
  hint: string;
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <label className="field span-all">
      <span>{label}</span>
      <span className="input-with-btn">
        <input
          value={value}
          maxLength={500}
          inputMode="url"
          placeholder="https://file.msboost.de/#/?code=…"
          onChange={(e) => onChange(e.target.value)}
        />
        <Button
          disabled={!https(value)}
          onClick={() =>
            window.open(value.trim(), "_blank", "noopener,noreferrer")
          }
        >
          <ExternalLink size={15} />
          打开测试
        </Button>
      </span>
      <small>{hint}</small>
    </label>
  );
}

export function LauncherSettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, loading, reload } = useData("/api/admin/settings");
  const [values, setValues] = useState<Launcher | null>(null),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  useEffect(() => {
    if (data) setValues({ ...(data.settings || data).launcher });
  }, [data]);
  if (loading && !values) return <Notice>正在读取启动器设置…</Notice>;
  if (!values) return <ErrorNotice error={error || "启动器设置读取失败"} />;
  const set = (key: keyof Launcher, value: string) =>
    setValues({ ...values, [key]: value });
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setFailure("");
        setMessage("");
        try {
          const { version, releasedAt, downloadUrl, backupUrl, sha256, notes } =
            values;
          await post(
            "/api/admin/settings",
            {
              launcher: {
                version: version || "",
                releasedAt: releasedAt || "",
                downloadUrl: downloadUrl || "",
                backupUrl: backupUrl || "",
                sha256: sha256 || "",
                notes: notes || "",
              },
            },
            "PUT",
          );
          setMessage("启动器设置已保存");
          reload();
          onSaved();
        } catch (err) {
          setFailure((err as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Notice>
        这些信息显示在主页的「下载 MSBOOST 启动器」和控制台「MSBOOST
        启动器」页面，访客无需登录也能看到。下载地址留空表示未发布；启动器发新版时只需改这里。
      </Notice>
      <div className="form-grid mt24">
        <Field
          label="当前版本"
          value={values.version || ""}
          maxLength={32}
          placeholder="v1.0.2"
          hint="例如 v1.0.2；只能用字母、数字和 . - +"
          onChange={(e) => set("version", e.target.value)}
        />
        <Field
          label="更新日期"
          type="date"
          value={values.releasedAt || ""}
          onChange={(e) => set("releasedAt", e.target.value)}
        />
        <LinkField
          label="下载地址"
          hint="自建网盘的分享链接，只接受 https://，会保留 #/?code= 这类参数"
          value={values.downloadUrl || ""}
          onChange={(value) => set("downloadUrl", value)}
        />
        <LinkField
          label="备用下载地址（可选）"
          hint="填写后下载区多显示一个「备用下载」按钮"
          value={values.backupUrl || ""}
          onChange={(value) => set("backupUrl", value)}
        />
        <Field
          className="mono-input"
          label="SHA-256（可选）"
          value={values.sha256 || ""}
          maxLength={64}
          spellCheck={false}
          hint="zip 文件的校验值，64 位十六进制，显示在下载按钮下方"
          onChange={(e) => set("sha256", e.target.value)}
        />
        <label className="field span-all">
          <span>更新内容（可选，支持 Markdown）</span>
          <textarea
            className="notes-input"
            maxLength={5000}
            value={values.notes || ""}
            onChange={(e) => set("notes", e.target.value)}
          />
          <small>显示在下载区「更新内容」里，最多 5000 字；留空则不显示</small>
        </label>
      </div>
      {failure && <Notice tone="red">{failure}</Notice>}
      {message && <Notice tone="green">{message}</Notice>}
      <div className="form-actions">
        <span className="save-meta">
          {values.updatedAt
            ? `最后保存：${date(values.updatedAt)}`
            : "还没有保存过"}
        </span>
        <a className="btn" href="/home#launcher" target="_blank" rel="noopener">
          <Eye size={15} />
          查看主页下载区
        </a>
        <Button type="submit" primary disabled={busy}>
          <Save size={15} />
          {busy ? "正在保存…" : "保存"}
        </Button>
      </div>
    </form>
  );
}
