import React, { useEffect, useState } from "react";
import {
  ArrowDown,
  ArrowLeft,
  ArrowRight,
  BookOpen,
  Check as CheckIcon,
  CircleCheck,
  CircleX,
  Download,
  Gamepad2,
  Gauge,
  Laptop,
  Lock,
  Megaphone,
  MonitorDown,
  Paperclip,
  Pin,
  Route,
  Server,
  Share2,
  ShieldCheck,
} from "lucide-react";
import { array, downloadFile, RecordData } from "./api";
import {
  Badge,
  Brand,
  Button,
  Empty,
  ErrorNotice,
  HeroTitle,
  Loading,
  Maple,
  Markdown,
  useData,
} from "./ui";
import {
  Launcher,
  LauncherDownload,
  LauncherNotes,
  LauncherSteps,
  LauncherTips,
} from "./launcher";
import { Agreements } from "./agreements";
import { SiteSwitcher } from "./site-switcher";
import "./public-site.css";

type Go = (href: string, event?: React.MouseEvent<HTMLAnchorElement>) => void;

const articlePath = /^\/news\/([A-Za-z0-9_-]{1,80})$/;

function readLocation() {
  return {
    path: location.pathname.replace(/\/+$/, "") || "/",
    search: location.search,
  };
}

// A minimal history router for the two public sections. The console keeps its
// hash navigation, so links to "/" always load it normally.
function usePublicLocation(): [string, string, Go] {
  const [current, setCurrent] = useState(readLocation);
  useEffect(() => {
    const pop = () => setCurrent(readLocation());
    addEventListener("popstate", pop);
    return () => removeEventListener("popstate", pop);
  }, []);
  const go: Go = (href, event) => {
    if (
      event &&
      (event.defaultPrevented ||
        event.button !== 0 ||
        event.metaKey ||
        event.ctrlKey ||
        event.shiftKey ||
        event.altKey)
    )
      return;
    if (!/^\/(home|news)(\/|\?|$)/.test(href)) return;
    event?.preventDefault();
    history.pushState(null, "", href);
    setCurrent(readLocation());
    scrollTo(0, 0);
  };
  return [current.path, current.search, go];
}

function PublicLink({
  href,
  go,
  ...props
}: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string; go: Go }) {
  const external = href.startsWith("https://");
  return (
    <a
      {...props}
      href={href}
      rel={external ? "noopener noreferrer" : undefined}
      onClick={(event) => go(href, event)}
    />
  );
}

function useTitle(title?: string) {
  useEffect(() => {
    if (title) document.title = title;
  }, [title]);
}

const day = (n: number) => (n ? new Date(n).toLocaleDateString("zh-CN") : "—");

export default function PublicSite() {
  const [path, search, go] = usePublicLocation();
  const [agreement, setAgreement] = useState("");
  const shell = useData("/api/public/home");
  const site = shell.data?.site || {};
  const article = path.match(articlePath);
  let page: React.ReactNode;
  if (article)
    page = <NewsArticle key={article[1]} id={article[1]} site={site} go={go} />;
  else if (path === "/news")
    page = (
      <NewsList
        category={new URLSearchParams(search).get("category") || ""}
        site={site}
        go={go}
      />
    );
  else if (path.startsWith("/news/")) page = <NewsMissing go={go} />;
  else
    page = (
      <HomePage
        data={shell.data}
        error={shell.error}
        loading={shell.loading}
        go={go}
      />
    );
  return (
    <div className="login-body public-site">
      <header className="public-top public-header">
        <a
          href="/home"
          className="public-brand"
          aria-label={`${site.siteName || "MSBOOST"} 主页`}
          onClick={(event) => go("/home", event)}
        >
          <Brand />
        </a>
        <SiteSwitcher
          current={path.startsWith("/news") ? "news" : "home"}
          onNavigate={go}
        />
      </header>
      <main className="public-main">{page}</main>
      <footer className="public-footer">
        <div className="public-contact">
          <span>
            © {new Date().getFullYear()} {site.siteName || "MSBOOST"}
          </span>
          {shell.data?.home?.contactEmail && (
            <a href={`mailto:${shell.data.home.contactEmail}`}>
              {shell.data.home.contactEmail}
            </a>
          )}
        </div>
        <div className="flex">
          <button type="button" onClick={() => setAgreement("terms")}>
            用户协议
          </button>
          <button type="button" onClick={() => setAgreement("privacy")}>
            隐私政策
          </button>
        </div>
      </footer>
      {agreement && (
        <Agreements kind={agreement} onClose={() => setAgreement("")} />
      )}
    </div>
  );
}

// Card targets that scroll to a section of this page.
const homeSections = new Set(["msboost", "relay", "launcher"]);
const sectionIcons: Record<string, typeof Server> = {
  "#msboost": Server,
  "#relay": Route,
  "#launcher": MonitorDown,
};
const featureIcons = [
  Server,
  Route,
  MonitorDown,
  ShieldCheck,
  BookOpen,
  Download,
];
const jumpLabels: Record<string, string> = {
  "#msboost": "了解 MSBOOST",
  "#relay": "了解中转服务器",
  "#launcher": "了解 MSBOOST 启动器",
};

// Sections declare a scroll margin for the sticky header; the address keeps
// the anchor so a shared link opens at the same place.
function scrollToSection(id: string, event?: React.MouseEvent) {
  const section = document.getElementById(id);
  if (!section) return;
  event?.preventDefault();
  section.scrollIntoView({ behavior: "smooth", block: "start" });
  history.replaceState(
    history.state,
    "",
    `${location.pathname}${location.search}#${id}`,
  );
}

// Explanation texts may use **bold** and line breaks; everything else is text.
function rich(text = "") {
  return text.split("\n").map((line, i) => (
    <React.Fragment key={i}>
      {i > 0 && <br />}
      {line
        .split(/\*\*(.+?)\*\*/g)
        .map((part, j) => (j % 2 ? <b key={j}>{part}</b> : part))}
    </React.Fragment>
  ));
}

type HomeItem = { title: string; desc?: string };
const routeIcons: Record<string, typeof Server> = {
  computer: Laptop,
  relay: Route,
  exit: Server,
  game: Gamepad2,
};

function RouteFlow({
  label,
  nodes,
  kinds,
}: {
  label: string;
  nodes: HomeItem[];
  kinds: string[];
}) {
  return (
    <div className="route-row">
      <span className="route-label">{label}</span>
      <div className="route-flow">
        {nodes.map((node, i) => {
          const kind = kinds[i] || "game",
            Icon = routeIcons[kind];
          return (
            <React.Fragment key={i}>
              {i > 0 && (
                <span className="route-arrow" aria-hidden="true">
                  <ArrowRight size={16} />
                </span>
              )}
              <div
                className={`route-node ${kind === "relay" || kind === "exit" ? kind : ""}`}
              >
                <Icon size={20} aria-hidden="true" />
                <strong>{node.title}</strong>
                {node.desc && <small>{node.desc}</small>}
              </div>
            </React.Fragment>
          );
        })}
      </div>
    </div>
  );
}

function ExplainHead({
  icon: Icon,
  eyebrow,
  title,
}: {
  icon: typeof Server;
  eyebrow?: string;
  title: string;
}) {
  return (
    <div className="explain-head">
      <span className="explain-icon">
        <Icon size={22} aria-hidden="true" />
      </span>
      <div>
        {eyebrow && <div className="eyebrow">{eyebrow}</div>}
        <h2>{title}</h2>
      </div>
    </div>
  );
}

function ExplainPoints({
  points,
  icons,
}: {
  points: HomeItem[];
  icons: (typeof Server)[];
}) {
  return (
    <div className="explain-points">
      {points.map((point, i) => {
        const Icon = icons[i % icons.length];
        return (
          <div className="explain-point" key={i}>
            <Icon size={20} aria-hidden="true" />
            <h3>{point.title}</h3>
            {point.desc && <p>{point.desc}</p>}
          </div>
        );
      })}
    </div>
  );
}

function MSBOOSTSection({
  msboost,
  launcher,
}: {
  msboost: RecordData;
  launcher: boolean;
}) {
  return (
    <section className="home-explain" id="msboost">
      <ExplainHead
        icon={Server}
        eyebrow={msboost.eyebrow}
        title={msboost.title}
      />
      {msboost.lead && <p className="explain-lead">{rich(msboost.lead)}</p>}
      <div className="route-map">
        <RouteFlow
          label="连接方式"
          nodes={msboost.nodes || []}
          kinds={["computer", "exit", "game"]}
        />
      </div>
      <ExplainPoints
        points={msboost.points || []}
        icons={[ShieldCheck, Server, MonitorDown]}
      />
      <div className="explain-actions">
        <a className="btn primary" href="/#deploy">
          去部署 MSBOOST
          <ArrowRight size={15} aria-hidden="true" />
        </a>
        {launcher && (
          <a
            className="btn"
            href="#launcher"
            onClick={(event) => scrollToSection("launcher", event)}
          >
            下载启动器
            <ArrowDown size={15} aria-hidden="true" />
          </a>
        )}
      </div>
    </section>
  );
}

function RelaySection({ relay }: { relay: RecordData }) {
  return (
    <section className="home-explain" id="relay">
      <ExplainHead icon={Route} eyebrow={relay.eyebrow} title={relay.title} />
      {relay.lead && <p className="explain-lead">{rich(relay.lead)}</p>}
      <div className="route-map">
        <RouteFlow
          label="直连"
          nodes={relay.direct || []}
          kinds={["computer", "exit", "game"]}
        />
        <RouteFlow
          label="经过中转"
          nodes={relay.via || []}
          kinds={["computer", "relay", "exit", "game"]}
        />
      </div>
      {(relay.myth || relay.truth) && (
        <div className="myth">
          {relay.myth && (
            <div className="myth-card is-wrong">
              <h3>
                <CircleX size={16} aria-hidden="true" />
                常见误解
              </h3>
              {rich(relay.myth)}
            </div>
          )}
          {relay.truth && (
            <div className="myth-card is-right">
              <h3>
                <CircleCheck size={16} aria-hidden="true" />
                实际上
              </h3>
              {rich(relay.truth)}
            </div>
          )}
        </div>
      )}
      <ExplainPoints
        points={relay.points || []}
        icons={[Lock, Share2, Gauge]}
      />
      <div className="explain-actions">
        <a className="btn primary" href="/#relay">
          去配置中转服务器
          <ArrowRight size={15} aria-hidden="true" />
        </a>
      </div>
    </section>
  );
}

function LauncherSection({ launcher }: { launcher?: Launcher }) {
  return (
    <section className="home-explain home-launcher" id="launcher">
      <ExplainHead
        icon={MonitorDown}
        eyebrow="MSBOOST 启动器"
        title="下载 MSBOOST 启动器"
      />
      <p className="explain-lead">
        导入部署或中转得到的配置文件，一键加速
        MapleStory。只代理游戏相关进程，其他软件照常直连。
      </p>
      <div className="mt16">
        <LauncherDownload launcher={launcher} heading="h3" />
      </div>
      {launcher?.downloadUrl && launcher.notes && (
        <details className="notes-details">
          <summary>{launcher.version} 更新内容</summary>
          <LauncherNotes launcher={launcher} />
        </details>
      )}
      <LauncherSteps />
      <LauncherTips />
    </section>
  );
}

function HomePage({
  data,
  error,
  loading,
  go,
}: {
  data: RecordData | null;
  error: string;
  loading: boolean;
  go: Go;
}) {
  useTitle(data?.site?.homeTitle);
  const ready = !!data;
  // A shared /home#relay link lands on its section once the page is built.
  useEffect(() => {
    const id = location.hash.slice(1);
    if (ready && homeSections.has(id))
      requestAnimationFrame(() =>
        document.getElementById(id)?.scrollIntoView(),
      );
  }, [ready]);
  if (!data) return loading ? <Loading /> : <ErrorNotice error={error} />;
  const home = data.home || {},
    articles = array(data, "articles"),
    launcher: Launcher | undefined = data.launcher,
    features: RecordData[] = home.features || [],
    points: string[] = home.points || [],
    msboost: RecordData = home.msboost || {},
    relay: RecordData = home.relay || {};
  const shown: Record<string, boolean> = {
    "#msboost": !!home.showExplain && !!msboost.title,
    "#relay": !!home.showExplain && !!relay.title,
    "#launcher": !!home.showLauncher,
  };
  return (
    <>
      {home.notice && (
        <div className="public-notice">
          <Megaphone size={16} aria-hidden="true" />
          {home.noticeHref ? (
            <PublicLink href={home.noticeHref} go={go}>
              {home.notice}
            </PublicLink>
          ) : (
            <span>{home.notice}</span>
          )}
        </div>
      )}
      <section className="hero public-hero">
        <div>
          {home.eyebrow && <div className="eyebrow">{home.eyebrow}</div>}
          <HeroTitle first={home.titleLine1} second={home.titleLine2} />
          {home.subtitle && (
            <p className="v4-hero-copy">
              {String(home.subtitle)
                .split("\n")
                .map((line, i) => (
                  <React.Fragment key={i}>
                    {i > 0 && <br />}
                    {line}
                  </React.Fragment>
                ))}
            </p>
          )}
          {points.length > 0 && (
            <div className="hero-points">
              {points.map((point) => (
                <span key={point}>
                  <CheckIcon size={13} /> {point}
                </span>
              ))}
            </div>
          )}
          {(home.primaryCta?.text || home.secondaryCta?.text) && (
            <div className="public-cta">
              {home.primaryCta?.text && (
                <PublicLink
                  className="btn primary"
                  href={home.primaryCta.href}
                  go={go}
                >
                  {home.primaryCta.text}
                  <ArrowRight size={15} aria-hidden="true" />
                </PublicLink>
              )}
              {home.secondaryCta?.text && (
                <PublicLink
                  className="btn"
                  href={home.secondaryCta.href}
                  go={go}
                >
                  {home.secondaryCta.text}
                </PublicLink>
              )}
            </div>
          )}
        </div>
        <div className="card public-steps">
          <div className="eyebrow">HOW IT WORKS</div>
          <h2>三步开始</h2>
          <div className="hero-steps">
            <div>
              <Server size={24} />
              <strong>准备自己的 VPS</strong>
            </div>
            <ArrowRight size={14} />
            <div className="active">
              <Maple />
              <strong>部署 MSBOOST</strong>
            </div>
            <ArrowRight size={14} />
            {shown["#launcher"] ? (
              <a
                className="step-link"
                href="#launcher"
                onClick={(event) => scrollToSection("launcher", event)}
              >
                <MonitorDown size={24} />
                <strong>下载启动器</strong>
              </a>
            ) : (
              <div>
                <MonitorDown size={24} />
                <strong>下载启动器</strong>
              </div>
            )}
          </div>
        </div>
      </section>
      {features.length > 0 && (
        <section className="public-tools">
          <div className="grid3">
            {features.map((feature, i) => {
              const target: string = feature.target || "",
                section = target.startsWith("#") ? target.slice(1) : "",
                Icon =
                  sectionIcons[target] || featureIcons[i % featureIcons.length],
                clickable = section ? !!shown[target] : !!target,
                body = (
                  <>
                    <span className="tool-icon">
                      <Icon size={22} />
                    </span>
                    <div>
                      <h3>{feature.title}</h3>
                      {feature.desc && <p>{feature.desc}</p>}
                      {clickable && (
                        <span className="card-jump">
                          {jumpLabels[target] || "了解更多"}
                          {section ? (
                            <ArrowDown size={13} aria-hidden="true" />
                          ) : (
                            <ArrowRight size={13} aria-hidden="true" />
                          )}
                        </span>
                      )}
                    </div>
                  </>
                );
              if (!clickable)
                return (
                  <div className="card tool-card" key={i}>
                    {body}
                  </div>
                );
              if (section)
                return (
                  <a
                    className="card tool-card is-link"
                    key={i}
                    href={target}
                    onClick={(event) => scrollToSection(section, event)}
                  >
                    {body}
                  </a>
                );
              return (
                <PublicLink
                  className="card tool-card is-link"
                  key={i}
                  href={target}
                  go={go}
                  target={target.startsWith("https://") ? "_blank" : undefined}
                >
                  {body}
                </PublicLink>
              );
            })}
          </div>
        </section>
      )}
      {shown["#msboost"] && (
        <MSBOOSTSection msboost={msboost} launcher={shown["#launcher"]} />
      )}
      {shown["#relay"] && <RelaySection relay={relay} />}
      {shown["#launcher"] && <LauncherSection launcher={launcher} />}
      {articles.length > 0 && (
        <section className="public-latest" aria-labelledby="latest-heading">
          <div className="between">
            <h2 id="latest-heading">最新公告与教程</h2>
            <PublicLink href="/news" go={go} className="public-more">
              查看全部 <ArrowRight size={14} aria-hidden="true" />
            </PublicLink>
          </div>
          <div className="news-grid">
            {articles.map((article) => (
              <PublicLink
                key={article.id}
                href={`/news/${article.id}`}
                go={go}
                className="card news-card"
              >
                <span className="news-card-meta">
                  <Badge tone="orange">{article.category || "文章"}</Badge>
                  {article.pinned && (
                    <Pin
                      className="news-pin"
                      size={13}
                      fill="currentColor"
                      aria-label="已固定"
                    />
                  )}
                </span>
                <h3>{article.title}</h3>
                {article.summary && <p>{article.summary}</p>}
                <small>{day(article.createdAt)}</small>
              </PublicLink>
            ))}
          </div>
        </section>
      )}
    </>
  );
}

function NewsHeading() {
  return (
    <div className="page-heading">
      <div>
        <div className="eyebrow">MSBOOST NEWS</div>
        <h1>公告与教程</h1>
        <p>无需登录即可阅读。部署和管理节点请进入控制台。</p>
      </div>
    </div>
  );
}

function NewsList({
  category,
  site,
  go,
}: {
  category: string;
  site: RecordData;
  go: Go;
}) {
  const { data, error, loading } = useData("/api/public/articles");
  useTitle(site.newsTitle);
  const articles = array(data, "articles"),
    categories = [
      ...new Set(articles.map((a) => String(a.category || "")).filter(Boolean)),
    ],
    shown = category
      ? articles.filter((a) => a.category === category)
      : articles;
  let body: React.ReactNode;
  if (!data) body = loading ? <Loading /> : <ErrorNotice error={error} />;
  else if (!data.enabled)
    body = (
      <div className="card news-closed">
        <Empty>文章区暂未开放，登录控制台查看公告与教程。</Empty>
        <div className="actions">
          <a className="btn primary" href="/">
            进入控制台
          </a>
        </div>
      </div>
    );
  else
    body = (
      <>
        {categories.length > 0 && (
          <div className="news-filters" role="group" aria-label="分类筛选">
            {["", ...categories].map((c) => (
              <PublicLink
                key={c || "all"}
                href={c ? `/news?category=${encodeURIComponent(c)}` : "/news"}
                go={go}
                className={`news-chip ${c === category ? "active" : ""}`}
                aria-current={c === category ? "page" : undefined}
              >
                {c || "全部"}
              </PublicLink>
            ))}
          </div>
        )}
        {shown.length ? (
          <div className="card flush news-list">
            {shown.map((article) => (
              <PublicLink
                key={article.id}
                href={`/news/${article.id}`}
                go={go}
                className="news-row"
              >
                <div>
                  <strong>
                    {article.pinned && (
                      <Pin
                        className="news-pin"
                        size={14}
                        fill="currentColor"
                        aria-label="已固定"
                      />
                    )}
                    {article.title}
                  </strong>
                  {article.summary && <p>{article.summary}</p>}
                </div>
                <div className="news-row-meta">
                  <Badge tone="orange">{article.category || "文章"}</Badge>
                  <span>{day(article.createdAt)}</span>
                  {article.attachmentCount > 0 && (
                    <span aria-label={`${article.attachmentCount} 个附件`}>
                      <Paperclip size={12} aria-hidden="true" />
                      {article.attachmentCount}
                    </span>
                  )}
                </div>
              </PublicLink>
            ))}
          </div>
        ) : (
          <div className="card">
            <Empty>
              {category ? `“${category}”分类下暂无公开文章` : "暂无公开文章"}
            </Empty>
          </div>
        )}
      </>
    );
  return (
    <section className="news-page">
      <NewsHeading />
      {body}
    </section>
  );
}

function NewsMissing({ go }: { go: Go }) {
  useTitle("文章不存在或未公开 · MSBOOST");
  return (
    <section className="news-article">
      <div className="card">
        <Empty>文章不存在或未公开</Empty>
        <div className="actions">
          <PublicLink href="/news" go={go} className="btn">
            <ArrowLeft size={15} aria-hidden="true" />
            返回文章列表
          </PublicLink>
        </div>
      </div>
    </section>
  );
}

function NewsArticle({
  id,
  site,
  go,
}: {
  id: string;
  site: RecordData;
  go: Go;
}) {
  const { data, loading } = useData(`/api/public/articles/${id}`),
    [message, setMessage] = useState("");
  const article = data?.article;
  useTitle(
    article ? `${article.title} · ${site.siteName || "MSBOOST"}` : undefined,
  );
  if (!article) return loading ? <Loading /> : <NewsMissing go={go} />;
  const attachments: RecordData[] = article.attachments || [];
  return (
    <article className="news-article">
      <nav className="news-crumb" aria-label="当前位置">
        <PublicLink href="/news" go={go}>
          文章
        </PublicLink>
        {article.category && (
          <>
            <span aria-hidden="true">/</span>
            <PublicLink
              href={`/news?category=${encodeURIComponent(article.category)}`}
              go={go}
            >
              {article.category}
            </PublicLink>
          </>
        )}
      </nav>
      <h1>{article.title}</h1>
      <div className="news-meta">
        <Badge tone="orange">{article.category || "文章"}</Badge>
        <span>发布 {day(article.createdAt)}</span>
        {article.updatedAt > article.createdAt && (
          <span>更新 {day(article.updatedAt)}</span>
        )}
      </div>
      <div className="card news-body">
        <Markdown text={article.body} />
      </div>
      {attachments.length > 0 && (
        <div className="card news-attachments mt16">
          <h3>附件</h3>
          <ErrorNotice error={message} />
          {attachments.map((file) => (
            <div className="attachment" key={file.id}>
              <span>
                {file.name}{" "}
                <small>{Math.max(1, Math.ceil(file.size / 1024))} KB</small>
              </span>
              {data.attachmentsEnabled ? (
                <Button
                  onClick={() => {
                    setMessage("");
                    void downloadFile(
                      `/api/articles/${article.id}/attachments/${file.id}`,
                      file.name,
                    ).catch((e) => setMessage(e.message));
                  }}
                >
                  下载
                </Button>
              ) : (
                <small>附件下载已关闭</small>
              )}
            </div>
          ))}
        </div>
      )}
      <PublicLink href="/news" go={go} className="btn news-back">
        <ArrowLeft size={15} aria-hidden="true" />
        返回文章列表
      </PublicLink>
      <div className="card news-cta">
        <div>
          <h3>准备好了？</h3>
          <p>进入控制台，部署节点、配置中转或重装系统。</p>
        </div>
        <a className="btn primary" href="/">
          进入控制台
        </a>
      </div>
    </article>
  );
}
