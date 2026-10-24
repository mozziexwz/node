import React from "react";
import {
  Check,
  KeyRound,
  Laptop,
  MonitorDown,
  ShieldAlert,
  ShieldCheck,
} from "lucide-react";
import { Button, ErrorNotice, Header, Notice, useData } from "./ui";
import {
  Launcher,
  LauncherDownload,
  LauncherNotes,
  settingsTabKey,
} from "./launcher";

// MSBOOST Local is a Windows program that deploys from the member's own
// computer, so the SSH password never reaches this site. The website only
// offers the download and points to it from the pages that ask for SSH; every
// pointer stays hidden until the administrator publishes a download.

export function useLocalApp(): Launcher | undefined {
  const { data } = useData("/api/public/launcher");
  return data?.localApp?.downloadUrl ? data.localApp : undefined;
}

export function LocalAppSteps() {
  const steps: [string, React.ReactNode][] = [
    [
      "解压到启动器旁边",
      <>
        下载 zip，解压到启动器所在的文件夹，例如 <code>D:\MSBOOST\</code>
        。部署出的配置会直接出现在启动器里。
      </>,
    ],
    [
      "添加你的服务器",
      <>
        双击 <code>MSBOOST-Local.exe</code>
        （不需要管理员权限），点「添加服务器」，填写 IP、SSH
        端口和登录方式，核对后信任服务器指纹。
      </>,
    ],
    [
      "部署节点并开始加速",
      "选中服务器点「部署节点」，输入 SSH 密码。完成后打开启动器，选中新配置，点「开始加速」。",
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

export function LocalAppTips({ checksum = false }: { checksum?: boolean }) {
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
        <KeyRound size={16} />
        <span>
          <b>密码不保存</b>
          ：本地版只记住服务器地址和指纹，每次操作时再输入 SSH 密码或私钥口令。
        </span>
      </li>
      {checksum && (
        <li>
          <ShieldCheck size={16} />
          <span>
            <b>只从本站下载</b>
            ：请核对下载区的 SHA-256，不要使用别人发来的安装包。
          </span>
        </li>
      )}
    </ul>
  );
}

const yes = (
  <span className="yes">
    <Check size={15} aria-label="支持" />
  </span>
);
const compare: [string, React.ReactNode, React.ReactNode][] = [
  [
    "SSH 密码去哪里",
    <small>经本站执行机登录，只用于当次任务，不保存</small>,
    <small>只在你的电脑内存里，不经过本站</small>,
  ],
  ["部署 MSBOOST 节点", yes, yes],
  ["升级节点（保留配置）", yes, yes],
  ["取回配置文件", yes, yes],
  ["配置中转服务器（可加前置机）", yes, yes],
  [
    "帮朋友中转",
    <small>中转码</small>,
    <small>双方交换两段文字，不交出 SSH 或配置</small>,
  ],
  ["卸载清理", yes, yes],
  ["DD 系统", yes, <span className="no">暂不支持</span>],
  ["捐赠权益、枫叶、工单", yes, <span className="no">请在网站使用</span>],
];

function LocalAppCompare() {
  return (
    <>
      <div className="lc-compare">
        <div className="lc-th" />
        <div className="lc-th">网站</div>
        <div className="lc-th lc-local">MSBOOST 本地版</div>
        {compare.map(([name, site, local]) => (
          <React.Fragment key={name}>
            <div className="lc-rowname">{name}</div>
            <div>{site}</div>
            <div className="lc-local">{local}</div>
          </React.Fragment>
        ))}
      </div>
      <p className="lc-foot">
        两边装的节点和中转完全一样：用本地版部署的，也能在网站「配置中转服务器」里管理和清理，反过来也一样。用本地版做的操作不会出现在网站的任务记录和使用指南进度里。
      </p>
    </>
  );
}

// Shown at the top of the side column of the pages that ask for SSH.
export function LocalHint({
  kind,
  onNavigate,
}: {
  kind: "deploy" | "relay";
  onNavigate: (view: string) => void;
}) {
  if (!useLocalApp()) return null;
  return (
    <div className="card lc-hint">
      <div className="lc-hint-head">
        <span className="launcher-mark">
          <Laptop size={20} />
        </span>
        <h3>不想在网站填写 SSH？</h3>
      </div>
      <p>
        {kind === "deploy"
          ? "用 MSBOOST 本地版在自己的电脑上部署，SSH 密码只在你的电脑里使用，不经过本站。"
          : "本地版也能配置中转服务器（可加前置机），SSH 密码只在你的电脑里使用，不经过本站。"}
      </p>
      <ul>
        <li>
          <Check size={14} aria-hidden="true" />
          {kind === "deploy"
            ? "部署出的节点和这里完全一样"
            : "建好的中转和这里完全一样"}
        </li>
        <li>
          <Check size={14} aria-hidden="true" />
          {kind === "deploy"
            ? "配置直接存进启动器，不用下载再导入"
            : "帮朋友中转时，双方都不用交出 SSH"}
        </li>
      </ul>
      <div>
        <Button className="soft" onClick={() => onNavigate("local")}>
          <Laptop size={15} />
          了解 MSBOOST 本地版
        </Button>
      </div>
    </div>
  );
}

export function LocalAppPage({
  admin,
  onNavigate,
}: {
  admin: boolean;
  onNavigate: (view: string) => void;
}) {
  const { data, error, loading } = useData("/api/public/launcher");
  const localApp: Launcher | undefined = data?.localApp;
  const published = !!localApp?.downloadUrl;
  return (
    <>
      <Header
        title="MSBOOST 本地版"
        sub="在自己的电脑上部署和管理 MSBOOST 节点与中转服务器。SSH 密码只在你的电脑里使用，不经过本站。"
      />
      <ErrorNotice error={error} />
      {loading && !data ? (
        <Notice>正在读取本地版信息…</Notice>
      ) : (
        <LauncherDownload
          launcher={localApp}
          product="localApp"
          admin={admin}
          onSettings={() => {
            try {
              sessionStorage.setItem(settingsTabKey, "localApp");
            } catch {
              // The settings page then opens on its first tab.
            }
            onNavigate("settings");
          }}
        />
      )}
      {published && (
        <>
          <div className="card mt24">
            <div className="launcher-head">
              <h3>本地版和网站怎么选</h3>
            </div>
            <LocalAppCompare />
          </div>
          <div className="card mt24">
            <div className="launcher-head">
              <h3>三步开始使用</h3>
            </div>
            <LocalAppSteps />
            <div className="launcher-jump">
              <span>还没有启动器？</span>
              <Button className="small" onClick={() => onNavigate("launcher")}>
                <MonitorDown size={14} />
                下载 MSBOOST 启动器
              </Button>
            </div>
          </div>
          <div className={`mt24 ${localApp?.notes ? "launcher-grid" : ""}`}>
            {localApp?.notes && (
              <div className="card">
                <div className="launcher-head">
                  <h3>更新内容 · {localApp.version}</h3>
                </div>
                <LauncherNotes launcher={localApp} />
              </div>
            )}
            <div className="card">
              <div className="launcher-head">
                <h3>安全提示</h3>
              </div>
              <LocalAppTips checksum={!!localApp?.sha256} />
            </div>
          </div>
        </>
      )}
    </>
  );
}
