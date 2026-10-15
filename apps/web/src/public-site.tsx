import React, { useEffect, useState } from "react";
import {
  ArrowLeft,
  ArrowRight,
  BookOpen,
  Check as CheckIcon,
  Download,
  Megaphone,
  Paperclip,
  Pin,
  RefreshCw,
  Route,
  Server,
  ShieldCheck,
} from "lucide-react";
import { array, downloadFile, RecordData } from "./api";
import {
  Badge,
  Brand,
  Button,
  Empty,
  ErrorNotice,
  Loading,
  Maple,
  Markdown,
  useData,
} from "./ui";
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

const featureIcons = [
  Server,
  Route,
  RefreshCw,
  ShieldCheck,
  BookOpen,
  Download,
];

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
  if (!data) return loading ? <Loading /> : <ErrorNotice error={error} />;
  const home = data.home || {},
    articles = array(data, "articles"),
    features: RecordData[] = home.features || [],
    points: string[] = home.points || [];
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
          <h1 className="v4-hero-title">
            <span className="v4-hero-line">{home.titleLine1}</span>
            {home.titleLine2 && (
              <span className="v4-hero-line orange">{home.titleLine2}</span>
            )}
          </h1>
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
            <div>
              <Download size={24} />
              <strong>下载配置并启动</strong>
            </div>
          </div>
        </div>
      </section>
      {features.length > 0 && (
        <section className="public-tools">
          <div className="grid3">
            {features.map((feature, i) => {
              const Icon = featureIcons[i % featureIcons.length];
              return (
                <div className="card tool-card" key={i}>
                  <span className="tool-icon">
                    <Icon size={22} />
                  </span>
                  <div>
                    <h3>{feature.title}</h3>
                    {feature.desc && <p>{feature.desc}</p>}
                  </div>
                </div>
              );
            })}
          </div>
        </section>
      )}
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
