import React from "react";
import {
  BookOpen,
  Check,
  ChevronRight,
  CircleCheck,
  CircleHelp,
  Clock,
  ExternalLink,
  Gamepad2,
  Laptop,
  LifeBuoy,
  Lightbulb,
  Monitor,
  MonitorDown,
  Package,
  RefreshCw,
  Route,
  Server,
  Share2,
  ShieldCheck,
  Sparkles,
  Ticket,
  TriangleAlert,
} from "lucide-react";
import { array, RecordData } from "./api";
import { Badge, Button, ErrorNotice, Header, useData } from "./ui";
import { openArticleKey } from "./navigation";
import { useLocalApp } from "./local-app";
import { launcherGuide } from "./launcher";

// The member's start page. Progress comes from their own task records; the
// website cannot tell whether the launcher was downloaded, so that step stays
// "next" once a node is deployed.

type Stage = "done" | "current" | "optional" | "todo";

const day = (time: number) =>
  time
    ? new Date(time).toLocaleDateString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
      })
    : "";

export function guideProgress(tasks: RecordData[]) {
  const succeeded = (kinds: string[]) =>
    tasks.filter((t) => kinds.includes(t.kind) && t.state === "succeeded");
  // Upgrading or fetching succeeds only on a server that has a node.
  const deployed = succeeded(["deploy", "upgrade", "fetch-config"]),
    relays = succeeded(["relay", "relay-code"]),
    deploying = tasks.some(
      (t) => t.kind === "deploy" && ["queued", "running"].includes(t.state),
    ),
    nodes = new Set(deployed.map((t) => t.host || t.id)).size,
    latest = Math.max(0, ...deployed.map((t) => Number(t.createdAt) || 0));
  const vps: Stage = deployed.length || deploying ? "done" : "current",
    deploy: Stage = deployed.length ? "done" : deploying ? "current" : "todo",
    relay: Stage = relays.length ? "done" : "optional",
    launcher: Stage = deployed.length ? "current" : "todo";
  return {
    stages: [vps, deploy, relay, launcher] as Stage[],
    done: [vps, deploy, relay].filter((s) => s === "done").length,
    nodes,
    latest,
    relays: relays.length,
    deploying: deploying && !deployed.length,
  };
}

const stageText: Record<Stage, string> = {
  done: "已完成",
  current: "下一步",
  optional: "可选 · 未配置",
  todo: "待完成",
};

function StepBadge({ stage, text }: { stage: Stage; text?: string }) {
  if (stage === "done") return <Badge tone="green">{text || "已完成"}</Badge>;
  if (stage === "current")
    return <Badge tone="orange">{text || "下一步"}</Badge>;
  return <Badge>{stage === "optional" ? "可选" : "待完成"}</Badge>;
}

function Step({
  index,
  stage,
  title,
  badge,
  children,
}: {
  index: number;
  stage: Stage;
  title: string;
  badge?: string;
  children: React.ReactNode;
}) {
  return (
    <section className={`g-step ${stage}`} aria-label={`第 ${index} 步`}>
      <div className="g-num" aria-hidden="true">
        {stage === "done" ? <Check size={22} /> : index}
      </div>
      <div className="card">
        <div className="g-step-head">
          <small>第 {index} 步</small>
          <h3>{title}</h3>
          <StepBadge stage={stage} text={badge} />
        </div>
        {children}
      </div>
    </section>
  );
}

const faqs: [string, string][] = [
  [
    "游戏服务器看到的是谁的 IP？",
    "始终是你自己 VPS 的 IP。中转服务器只中转，不做游戏出口，朋友分享给你的是线路，不是 IP。",
  ],
  [
    "中转服务器能看到我的游戏内容吗？",
    "看不到。电脑和节点之间的流量是加密的，中转服务器只按端口中转，也拿不到你的节点密码。",
  ],
  [
    "配置文件丢了怎么办？",
    "在当前浏览器的「任务记录」里再次下载。换了电脑或浏览器找不到时，用「部署 MSBOOST → 取回配置文件」从服务器重新读取，原配置继续能用。",
  ],
  [
    "Steam 版可以用吗？",
    "可以。先在启动器里开始加速，再照常从 Steam 启动 MapleStory，登录窗口和游戏都会走加速。",
  ],
  [
    "加速后延迟还是高？",
    "看启动器「网络检测」：到中转服务器这段高，换一台线路更好的中转；到游戏服务器整体高，多半是 VPS 离游戏服务器太远。",
  ],
  [
    "出问题时怎么反馈？",
    "在启动器「日志」页点「导出诊断包」，提交工单时附上。诊断包里有节点地址，只发给 MSBOOST 客服，不要公开发布。",
  ],
];

const tools: [string, string, string, typeof Server][] = [
  ["launcher", "MSBOOST 启动器", "下载与更新说明", MonitorDown],
  ["deploy", "部署 MSBOOST", "安装节点并生成配置", Server],
  ["relay", "配置中转服务器", "手动配置或使用中转码", Route],
  ["codes", "中转码管理", "分享线路、调整带宽", Share2],
  ["dd", "DD 系统", "在线重装 Debian 12", RefreshCw],
  ["tasks", "任务记录", "查看进度、再次下载配置", Clock],
];

const routes: {
  title: string;
  tag: string;
  tone?: string;
  flow: [string, typeof Server, string?][];
  text: string;
  steps: string;
}[] = [
  {
    title: "直连",
    tag: "推荐先试",
    tone: "orange",
    flow: [
      ["你的电脑", Monitor],
      ["你的节点", Server, "node"],
      ["MapleStory", Gamepad2],
    ],
    text: "VPS 本身线路好、延迟低时最简单，配置文件导入启动器就能用。",
    steps: "第 1 → 2 → 4 步",
  },
  {
    title: "自己的中转服务器",
    tag: "线路更稳",
    flow: [
      ["电脑", Monitor],
      ["中转", Route, "relay"],
      ["你的节点", Server, "node"],
      ["游戏", Gamepad2],
    ],
    text: "直连延迟高、晚上丢包时，用一台精品线路 VPS 在中间中转。",
    steps: "第 1 → 2 → 3 → 4 步",
  },
  {
    title: "朋友的中转码",
    tag: "朋友分享",
    flow: [
      ["电脑", Monitor],
      ["朋友中转", Share2, "relay"],
      ["你的节点", Server, "node"],
      ["游戏", Gamepad2],
    ],
    text: "朋友把线路分享给你：上传你的配置文件就能建好中转，不需要对方的 SSH。",
    steps: "第 1 → 2 → 3 → 4 步",
  },
];

export function Guide({ navigate }: { navigate: (view: string) => void }) {
  const tasks = useData("/api/tasks"),
    news = useData("/api/articles"),
    localApp = useLocalApp();
  const known = !!tasks.data && !tasks.error,
    progress = guideProgress(known ? array(tasks.data, "tasks") : []),
    [vps, deploy, relay, launcher] = known
      ? progress.stages
      : (["todo", "todo", "optional", "todo"] as Stage[]),
    latest = array(news.data, "articles").slice(0, 3);
  const progressItems: [string, Stage, string][] = [
    ["准备 VPS", vps, stageText[vps]],
    [
      "部署 MSBOOST 节点",
      deploy,
      deploy === "done"
        ? `已部署 ${progress.nodes} 台 · ${day(progress.latest)}`
        : progress.deploying
          ? "正在部署"
          : stageText[deploy],
    ],
    [
      "配置中转",
      relay,
      relay === "done" ? `已配置 ${progress.relays} 条` : stageText[relay],
    ],
    ["启动器加速", launcher, stageText[launcher]],
  ];
  const next = !known
    ? null
    : deploy === "done"
      ? {
          title: "下载 MSBOOST 启动器，导入配置开始加速",
          text: "配置文件在第 2 步完成后已下载，也可以在任务记录里再次下载。",
          action: "下载 MSBOOST 启动器",
          view: "launcher",
          icon: MonitorDown,
        }
      : progress.deploying
        ? {
            title: "节点正在部署",
            text: "部署通常需要几分钟。完成后在结果页或任务记录里下载配置文件。",
            action: "查看任务记录",
            view: "tasks",
            icon: Clock,
          }
        : {
            title: "准备一台 VPS，然后部署 MSBOOST 节点",
            text: "系统为 Debian 11–13、可以用 root 通过 SSH 登录即可。填写服务器信息后，网站自动安装并自测。",
            action: "部署 MSBOOST",
            view: "deploy",
            icon: Server,
          };
  const openArticle = (id: string) => {
    try {
      sessionStorage.setItem(openArticleKey, id);
    } catch {
      // The article page then opens on its first article.
    }
    navigate("tutorials");
  };
  return (
    <>
      <Header
        title="使用指南"
        sub="第一次使用？按顺序完成下面 4 步，就能用自己的节点加速 MapleStory。"
      >
        <Button onClick={() => navigate("tutorials")}>
          <BookOpen size={15} />
          公告/教程
        </Button>
      </Header>
      <ErrorNotice error={tasks.error} />
      <section className="card g-hero" aria-label="你的进度">
        <div className="g-hero-main">
          <h2>
            你的进度
            {known && (
              <Badge tone={progress.done ? "green" : ""}>
                {progress.done} / 4 已完成
              </Badge>
            )}
          </h2>
          <p>
            {known
              ? "根据你的任务记录自动更新。中转是可选步骤，直连够快可以跳过。"
              : tasks.error
                ? "暂时读不到任务记录，下面的步骤仍可按顺序参考。"
                : "正在读取你的任务记录…"}
          </p>
          <ol className="g-progress">
            {progressItems.map(([name, stage, text], i) => (
              <li className={stage} key={name}>
                <span className="g-dot" aria-hidden="true">
                  {stage === "done" ? <Check size={16} /> : i + 1}
                </span>
                <b>{name}</b>
                <small>{known ? text : "—"}</small>
              </li>
            ))}
          </ol>
        </div>
        {next && (
          <div className="g-next">
            <div className="eyebrow">下一步</div>
            <h3>{next.title}</h3>
            <p>{next.text}</p>
            <Button primary onClick={() => navigate(next.view)}>
              <next.icon size={15} />
              {next.action}
            </Button>
          </div>
        )}
      </section>

      <div className="g-section-title mt24">
        <h2>选择你的连接方式</h2>
        <span>不确定就先用直连，延迟高再加中转</span>
      </div>
      <div className="g-routes">
        {routes.map((route) => (
          <div className="card g-route" key={route.title}>
            <div className="g-route-head">
              <h3>{route.title}</h3>
              <Badge tone={route.tone || ""}>{route.tag}</Badge>
            </div>
            <div className="g-flow">
              {route.flow.map(([name, Icon, kind], i) => (
                <React.Fragment key={name}>
                  {i > 0 && <i aria-hidden="true">→</i>}
                  <span className={kind || ""}>
                    <Icon size={13} aria-hidden="true" />
                    {name}
                  </span>
                </React.Fragment>
              ))}
            </div>
            <p>{route.text}</p>
            <div className="g-route-steps">
              需要完成：<b>{route.steps}</b>
            </div>
          </div>
        ))}
      </div>

      <div className="g-layout">
        <div className="g-main">
          <div className="g-section-title">
            <h2>按步骤完成</h2>
            <span>每一步都可以随时回来查看</span>
          </div>
          <div className="g-steps">
            <Step index={1} stage={vps} title="准备一台 VPS">
              <div className="g-step-body">
                <div>
                  <p className="g-desc">
                    MSBOOST 节点装在你自己的 VPS 上，游戏服务器看到的就是这台
                    VPS 的 IP。
                  </p>
                  <ul className="g-checks">
                    <li>
                      <CircleCheck size={15} aria-hidden="true" />
                      <span>
                        系统为 Debian 11–13；不是的话，先在 VPS 控制面板重装为
                        Debian 12
                      </span>
                    </li>
                    <li>
                      <CircleCheck size={15} aria-hidden="true" />
                      <span>可以用 root 通过 SSH 登录（密码或私钥都可以）</span>
                    </li>
                  </ul>
                </div>
                <div className="g-tip">
                  <b>
                    <Lightbulb size={15} aria-hidden="true" />
                    小提示
                  </b>
                  还没买 VPS？先看「公告/教程」里的线路机 VPS 资讯。
                </div>
              </div>
              <div className="g-actions">
                <Button onClick={() => navigate("tutorials")}>
                  <BookOpen size={15} />
                  VPS 推荐
                </Button>
              </div>
            </Step>
            <Step
              index={2}
              stage={deploy}
              title="部署 MSBOOST 节点"
              badge={
                deploy === "done"
                  ? `已部署 ${progress.nodes} 台`
                  : progress.deploying
                    ? "部署中"
                    : undefined
              }
            >
              <div className="g-step-body">
                <div>
                  <p className="g-desc">
                    填写服务器 IP、SSH
                    端口和登录方式后提交。网站自动安装并自测，完成后下载配置文件。
                  </p>
                  <ul className="g-checks">
                    <li>
                      <CircleCheck size={15} aria-hidden="true" />
                      <span>完成后点「下载配置」，得到 .json 配置文件</span>
                    </li>
                    <li>
                      <CircleCheck size={15} aria-hidden="true" />
                      <span>也可以在当前浏览器的「任务记录」里再次下载</span>
                    </li>
                  </ul>
                  {localApp && (
                    <div className="lc-inline">
                      <Laptop size={15} aria-hidden="true" />
                      <span>
                        不想在网站填写 SSH？可以用 MSBOOST
                        本地版在自己电脑上部署。本地版的部署不会显示在这里的进度中，完成后直接进行第
                        4 步即可。
                      </span>
                    </div>
                  )}
                </div>
                <div className="g-tip warn">
                  <b>
                    <ShieldCheck size={15} aria-hidden="true" />
                    保管好配置文件
                  </b>
                  配置文件里有节点的连接密码，请备份好，不要发给别人。重新部署会生成新的配置，旧配置随之失效。
                </div>
              </div>
              <div className="g-actions">
                <Button primary onClick={() => navigate("deploy")}>
                  <Server size={15} />
                  部署 MSBOOST
                </Button>
                {localApp && (
                  <Button onClick={() => navigate("local")}>
                    <Laptop size={15} />用 MSBOOST 本地版
                  </Button>
                )}
                <Button onClick={() => navigate("tasks")}>
                  <Clock size={15} />
                  任务记录
                </Button>
              </div>
            </Step>
            <Step
              index={3}
              stage={relay}
              title="配置中转"
              badge={
                relay === "done" ? `已配置 ${progress.relays} 条` : undefined
              }
            >
              <div className="g-step-body">
                <div>
                  <p className="g-desc">
                    直连已经很快，可以跳过这一步。直连延迟高、晚上丢包时，让一台线路更好的服务器在中间中转。
                  </p>
                  <div className="g-choose">
                    {[
                      ["自己有中转服务器的 SSH", "手动配置中转"],
                      ["朋友给了你中转码", "使用中转码"],
                      ["想把自己的线路分享给朋友", "生成中转码"],
                    ].map(([when, what]) => (
                      <div key={what}>
                        <span>{when}</span>
                        <b>{what}</b>
                      </div>
                    ))}
                  </div>
                </div>
                <div className="g-tip">
                  <b>
                    <ShieldCheck size={15} aria-hidden="true" />
                    中转只中转
                  </b>
                  你的电脑和节点之间是加密的，中转服务器只按端口中转，看不到内容；游戏出口仍是你自己的节点。
                </div>
              </div>
              <div className="g-actions">
                <Button onClick={() => navigate("relay")}>
                  <Route size={15} />
                  配置中转服务器
                </Button>
                <Button onClick={() => navigate("codes")}>
                  <Share2 size={15} />
                  中转码管理
                </Button>
              </div>
            </Step>
            <Step index={4} stage={launcher} title="下载启动器，开始加速">
              <div className="g-step-body">
                <ol className="g-sub">
                  <li>
                    <span>
                      下载 zip，解压到一个新文件夹，例如{" "}
                      <code>D:\MSBOOST\</code>
                    </span>
                  </li>
                  <li>
                    <span>
                      双击 <code>MSBOOST-Launcher.exe</code>
                      ，在权限提示中点「是」
                    </span>
                  </li>
                  <li>
                    <span>
                      在「节点配置」页导入第 2 或第 3 步下载的 .json 配置文件
                    </span>
                  </li>
                  <li>
                    <span>
                      回到「加速」页点「开始加速」，再打开 MapleStory（Steam
                      版也一样）
                    </span>
                  </li>
                  <li>
                    <span>
                      在「网络检测」确认到游戏服务器的 IPv4 是你自己 VPS 的 IP
                    </span>
                  </li>
                </ol>
                <div className="g-app" aria-hidden="true">
                  <div className="g-app-bar">
                    <span>MSBOOST 启动器</span>
                    <i />
                    <i />
                    <i />
                  </div>
                  <div className="g-app-tabs">
                    {[
                      "加速",
                      "节点配置",
                      "网络检测",
                      "游戏与加速项",
                      "日志",
                      "设置与关于",
                    ].map((tab, i) => (
                      <span className={i ? "" : "on"} key={tab}>
                        {tab}
                      </span>
                    ))}
                  </div>
                  <div className="g-app-body">
                    <div>
                      <em>节点配置</em>
                      <b>我的节点.json</b>
                    </div>
                    <div>
                      <em>可选加速</em>
                      <b>Discord · Steam</b>
                    </div>
                    <div>
                      <em>网络检测</em>
                      <b className="ok">MSBOOST IPv4 · 中转 · 游戏 正常</b>
                    </div>
                  </div>
                  <div className="g-app-foot">
                    <span>
                      <i />
                      已就绪
                    </span>
                    <b>开始加速</b>
                  </div>
                </div>
              </div>
              <div className="g-tip mt16">
                <b>
                  <TriangleAlert size={15} aria-hidden="true" />被 Windows
                  或杀毒软件拦截？
                </b>
                程序没有代码签名，SmartScreen 提示「Windows
                已保护你的电脑」时，点「更多信息」→「仍要运行」。启动器带网络分流驱动并需要管理员权限，部分杀毒软件可能误报，可以把程序文件夹加入白名单。
              </div>
              <div className="g-actions">
                <Button primary onClick={() => navigate("launcher")}>
                  <MonitorDown size={15} />
                  下载 MSBOOST 启动器
                </Button>
                <a
                  className="btn"
                  href={launcherGuide}
                  target="_blank"
                  rel="noopener"
                >
                  <BookOpen size={15} />
                  启动器使用指南
                  <ExternalLink size={13} aria-hidden="true" />
                </a>
              </div>
            </Step>
          </div>

          <div className="g-section-title mt24">
            <h2>常见问题</h2>
            <span>没找到答案可以提交工单</span>
          </div>
          <div className="g-faq">
            {faqs.map(([question, answer]) => (
              <div className="card" key={question}>
                <h4>
                  <CircleHelp size={16} aria-hidden="true" />
                  {question}
                </h4>
                <p>{answer}</p>
              </div>
            ))}
          </div>
        </div>

        <aside className="g-aside stack">
          <div className="card">
            <h3 className="g-card-title">
              <Sparkles size={17} aria-hidden="true" />
              常用工具
            </h3>
            <div className="g-links">
              {tools.map(([view, name, text, Icon]) => (
                <button type="button" key={view} onClick={() => navigate(view)}>
                  <span className="tool-icon">
                    <Icon size={18} aria-hidden="true" />
                  </span>
                  <span>
                    <b>{name}</b>
                    <small>{text}</small>
                  </span>
                  <ChevronRight size={16} aria-hidden="true" />
                </button>
              ))}
            </div>
          </div>
          <div className="card">
            <h3 className="g-card-title">
              <BookOpen size={17} aria-hidden="true" />
              最新公告与教程
              <button
                type="button"
                className="g-more"
                onClick={() => navigate("tutorials")}
              >
                查看全部 →
              </button>
            </h3>
            {latest.length ? (
              <ul className="g-news">
                {latest.map((article) => (
                  <li key={article.id}>
                    <button
                      type="button"
                      onClick={() => openArticle(article.id)}
                    >
                      <span>{article.title}</span>
                      <small>{day(article.createdAt)}</small>
                    </button>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="g-empty">
                {news.loading ? "正在读取…" : "暂无已发布的公告与教程"}
              </p>
            )}
          </div>
          <div className="card g-help">
            <h3 className="g-card-title">
              <LifeBuoy size={17} aria-hidden="true" />
              遇到问题？
            </h3>
            <p>
              先看左侧「常见问题」。仍未解决，在启动器「日志」页导出诊断包，提交工单时附上，客服会尽快回复。
            </p>
            <Button className="mt16" onClick={() => navigate("tickets")}>
              <Ticket size={15} />
              提交工单
            </Button>
          </div>
          <div className="card g-help">
            <h3 className="g-card-title">
              <Package size={17} aria-hidden="true" />
              捐赠权益
            </h3>
            <p>
              兑换枫叶后可使用站点提供的中转线路，并查看流量统计和线路状态。免费工具不受影响。
            </p>
            <Button className="mt16" onClick={() => navigate("plans")}>
              枫叶兑换
            </Button>
          </div>
        </aside>
      </div>
    </>
  );
}
