import React, { useState } from "react";
import { ArrowRight, Megaphone, Pencil, Save } from "lucide-react";
import { post } from "./api";
import {
  Badge,
  Button,
  Check,
  Field,
  Markdown,
  Modal,
  Notice,
  date,
} from "./ui";
import "./news-notice.css";

// The announcement at the top of 公告/教程. Administrators write it on that
// page; it can also open the public news page. The server sends it only to
// the readers who may see it.
export type NewsNotice = {
  enabled?: boolean;
  title?: string;
  body?: string;
  linkText?: string;
  linkHref?: string;
  public?: boolean;
  updatedAt?: number;
};

type LinkHandler = (
  href: string,
  event?: React.MouseEvent<HTMLAnchorElement>,
) => void;

export function NewsNoticeCard({
  notice,
  onLink,
  badges,
  actions,
}: {
  notice: NewsNotice;
  onLink?: LinkHandler;
  badges?: React.ReactNode;
  actions?: React.ReactNode;
}) {
  const href = notice.linkHref || "",
    external = href.startsWith("https://"),
    link = !!(notice.linkText && href);
  return (
    <div className="card na-card">
      <span className="na-icon">
        <Megaphone size={20} aria-hidden="true" />
      </span>
      <div className="na-main">
        <div className="na-title">
          {notice.title && <h3>{notice.title}</h3>}
          {notice.updatedAt ? (
            <small>
              {new Date(notice.updatedAt).toLocaleDateString("zh-CN")}
            </small>
          ) : null}
          {badges}
        </div>
        {notice.body && (
          <div className="na-body">
            <Markdown text={notice.body} />
          </div>
        )}
      </div>
      {(link || actions) && (
        <div className="na-actions">
          {link && (
            <a
              className="btn small primary"
              href={href}
              target={external ? "_blank" : undefined}
              rel={external ? "noopener noreferrer" : undefined}
              onClick={(event) => onLink?.(href, event)}
            >
              {notice.linkText}
              <ArrowRight size={14} aria-hidden="true" />
            </a>
          )}
          {actions}
        </div>
      )}
    </div>
  );
}

// The administrator's view: the notice with its state, or a placeholder.
export function AdminNewsNotice({
  notice,
  onSaved,
}: {
  notice?: NewsNotice;
  onSaved: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const current = notice || {};
  const written = !!(current.title || current.body);
  return (
    <>
      {written ? (
        <NewsNoticeCard
          notice={current}
          badges={
            <>
              <Badge tone={current.enabled ? "green" : ""}>
                {current.enabled ? "显示中" : "已隐藏"}
              </Badge>
              {current.enabled && current.public && (
                <Badge tone="orange">文章页也显示</Badge>
              )}
            </>
          }
          actions={
            <Button className="small" onClick={() => setEditing(true)}>
              <Pencil size={14} />
              编辑公告
            </Button>
          }
        />
      ) : (
        <div className="card na-empty">
          <span className="na-icon">
            <Megaphone size={20} aria-hidden="true" />
          </span>
          <div>
            <h3>还没有页面公告</h3>
            <p>
              写一条公告，会显示在「公告/教程」最上面，也可以同时显示在公开的文章页。没有公告时，会员和访客看不到这一块。
            </p>
          </div>
          <Button primary onClick={() => setEditing(true)}>
            <Pencil size={15} />
            写公告
          </Button>
        </div>
      )}
      {editing && (
        <NewsNoticeEditor
          notice={current}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            onSaved();
          }}
        />
      )}
    </>
  );
}

function NewsNoticeEditor({
  notice,
  onClose,
  onSaved,
}: {
  notice: NewsNotice;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [values, setValues] = useState<NewsNotice>(() => ({
      enabled: notice.title || notice.body ? !!notice.enabled : true,
      title: notice.title || "",
      body: notice.body || "",
      linkText: notice.linkText || "",
      linkHref: notice.linkHref || "",
      public: !!notice.public,
    })),
    [busy, setBusy] = useState(false),
    [failure, setFailure] = useState("");
  const set = (key: keyof NewsNotice, value: string | boolean) =>
    setValues({ ...values, [key]: value });
  return (
    <Modal title="编辑页面公告" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (busy) return;
          setBusy(true);
          setFailure("");
          try {
            await post("/api/admin/settings", { newsNotice: values }, "PUT");
            onSaved();
          } catch (err) {
            setFailure((err as Error).message);
            setBusy(false);
          }
        }}
      >
        <Check
          label="显示公告"
          checked={!!values.enabled}
          onChange={(e) => set("enabled", e.target.checked)}
        />
        <div className="form-grid mt16">
          <label className="field span-all">
            <span>标题</span>
            <input
              maxLength={40}
              value={values.title}
              onChange={(e) => set("title", e.target.value)}
            />
            <small>最多 40 字</small>
          </label>
          <label className="field span-all">
            <span>内容（支持 Markdown）</span>
            <textarea
              className="notes-input"
              maxLength={600}
              value={values.body}
              onChange={(e) => set("body", e.target.value)}
            />
            <small>最多 600 字，可以加粗、换行和加链接，不支持图片</small>
          </label>
          <Field
            label="按钮文字（可选）"
            maxLength={12}
            value={values.linkText}
            onChange={(e) => set("linkText", e.target.value)}
          />
          <Field
            label="按钮链接（可选）"
            placeholder="/#local、/news/… 或 https://"
            hint="两项都填才显示按钮"
            value={values.linkHref}
            onChange={(e) => set("linkHref", e.target.value)}
          />
        </div>
        <Check
          label="同时显示在公开的文章页（访客不用登录也能看到）"
          checked={!!values.public}
          onChange={(e) => set("public", e.target.checked)}
        />
        {failure && <Notice tone="red">{failure}</Notice>}
        <div className="form-actions">
          <span className="save-meta">
            {notice.updatedAt
              ? `最后保存：${date(notice.updatedAt)}`
              : "还没有保存过"}
          </span>
          <Button onClick={onClose}>取消</Button>
          <Button type="submit" primary disabled={busy}>
            <Save size={15} />
            {busy ? "正在保存…" : "保存公告"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
