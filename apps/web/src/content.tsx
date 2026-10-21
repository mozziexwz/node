import { Fragment, Suspense, lazy, useEffect, useRef, useState } from "react";
import {
  ArrowUp,
  ArrowDown,
  ArrowUpToLine,
  ArrowDownToLine,
  Bold,
  Italic,
  Strikethrough,
  Highlighter,
  Heading,
  List,
  ListOrdered,
  Quote,
  Link,
  Image,
  Code,
  Table2,
  Minus,
  Undo2,
  Redo2,
  Eraser,
  Maximize,
  Minimize,
  Keyboard,
  LoaderCircle,
  Pin,
  ExternalLink,
} from "lucide-react";
import { api, post, array, downloadFile, RecordData } from "./api";
import {
  Header,
  Button,
  Field,
  Select,
  Check,
  Notice,
  Table,
  Modal,
  Badge,
  ErrorNotice,
  Empty,
  AsyncForm,
  useData,
  date,
  Markdown,
} from "./ui";
import type {
  MarkdownAction,
  MarkdownEditorHandle,
  MarkdownEditorProps,
} from "./markdown-editor";
import { openArticleKey } from "./navigation";
import "./content-extra.css";

// CodeMirror is only downloaded when an administrator opens the editor. If it
// cannot be loaded, for example after an upgrade replaced the files, a plain
// text box keeps the article editable.
const MarkdownEditor = lazy(() =>
  import("./markdown-editor").catch(() => ({ default: PlainEditor })),
);
function PlainEditor({
  value,
  onChange,
  disabled,
  label,
}: MarkdownEditorProps) {
  return (
    <textarea
      className="markdown-plain"
      aria-label={label || "文章 Markdown 正文"}
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}
const formatGroups: [string, MarkdownAction, typeof Bold, string?][][] = [
  [
    ["加粗", "bold", Bold, "Ctrl+B"],
    ["斜体", "italic", Italic, "Ctrl+I"],
    ["删除线", "strike", Strikethrough],
    ["高亮", "highlight", Highlighter],
  ],
  [
    ["标题", "heading", Heading],
    ["无序列表", "bullet", List],
    ["有序列表", "ordered", ListOrdered],
    ["引用", "quote", Quote],
  ],
  [
    ["链接", "link", Link, "Ctrl+K"],
    ["图片", "image", Image],
    ["代码", "code", Code],
    ["表格", "table", Table2],
    ["分隔线", "rule", Minus],
  ],
];
const shortcuts = [
  ["加粗", "Ctrl B"],
  ["斜体", "Ctrl I"],
  ["插入链接", "Ctrl K"],
  ["撤销 / 重做", "Ctrl Z / Ctrl Y"],
  ["列表缩进 / 取消缩进", "Tab / Shift Tab"],
  ["续写列表，空项回车结束", "Enter"],
  ["保存", "Ctrl S"],
];
export function Articles({ admin = false }: { admin?: boolean }) {
  const { data, error, reload, setData } = useData(
      admin ? "/api/admin/articles" : "/api/articles",
    ),
    [active, setActive] = useState(() => {
      // Opened from a link to one article, such as the guide's latest list.
      try {
        const id = sessionStorage.getItem(openArticleKey) || "";
        sessionStorage.removeItem(openArticleKey);
        return id;
      } catch {
        return "";
      }
    }),
    [editing, setEditing] = useState<RecordData | null>(null),
    [tab, setTab] = useState("content"),
    [message, setMessage] = useState(""),
    [expanded, setExpanded] = useState(false),
    [editingBusy, setEditingBusy] = useState(false),
    [editorMessage, setEditorMessage] = useState(""),
    [moving, setMoving] = useState(false),
    [tools, setTools] = useState<MarkdownEditorHandle | null>(null),
    [undoable, setUndoable] = useState([false, false]),
    [previewPanel, setPreviewPanel] = useState<HTMLDivElement | null>(null);
  const editSession = useRef(0),
    editMutation = useRef(false);
  useEffect(() => {
    if (!expanded) return;
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        event.stopImmediatePropagation();
        setExpanded(false);
      }
    };
    window.addEventListener("keydown", escape, true);
    return () => window.removeEventListener("keydown", escape, true);
  }, [expanded]);
  const items = array(data, "articles"),
    current = items.find((x) => x.id === active) || items[0];
  function openEditor(article: RecordData | null) {
    editSession.current++;
    setEditorMessage("");
    setTools(null);
    setUndoable([false, false]);
    setEditing(article);
  }
  async function remove(v: RecordData) {
    if (
      !confirm(
        `删除“${v.title}”（${v.published ? "已发布" : "草稿"}，${v.attachments?.length || 0} 个附件）？`,
      )
    )
      return;
    try {
      await api("/api/admin/articles/" + v.id, { method: "DELETE" });
      if (editing?.id === v.id) setEditing(null);
      reload();
    } catch (e) {
      setMessage((e as Error).message);
    }
  }
  async function move(id: string, direction: "up" | "down" | "top" | "bottom") {
    if (moving) return;
    setMoving(true);
    setMessage("");
    try {
      const result = await post(`/api/admin/articles/${id}/move`, {
        direction,
      });
      setData(result);
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setMoving(false);
    }
  }
  // Publishing and guest visibility change through a dedicated endpoint so a
  // list row can never write a stale title or body back.
  async function visibility(
    article: RecordData,
    change: { published?: boolean; public?: boolean },
  ) {
    if (moving) return;
    setMoving(true);
    setMessage("");
    try {
      const result = await post(
        `/api/admin/articles/${article.id}/visibility`,
        change,
      );
      setData(result);
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setMoving(false);
    }
  }
  async function pin(article: RecordData) {
    if (moving) return;
    setMoving(true);
    setMessage("");
    try {
      const result = await post(`/api/admin/articles/${article.id}/pin`, {
        pinned: !article.pinned,
      });
      setData(result);
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setMoving(false);
    }
  }
  return (
    <>
      <Header
        title="公告/教程"
        sub={
          admin
            ? "新文章默认在前；固定文章优先显示，上下排序在各组内调整。"
            : "先了解操作流程，再开始部署。"
        }
      >
        {admin && (
          <Button
            primary
            disabled={editingBusy}
            onClick={() =>
              openEditor({
                title: "",
                body: "",
                category: "教程",
                summary: "",
                published: false,
                public: false,
                sort: 0,
              })
            }
          >
            新建文章
          </Button>
        )}
      </Header>
      <ErrorNotice error={error || message} />
      {admin ? (
        <div className="card flush">
          <Table
            headers={["标题", "分类", "状态", "访客可见", "更新", "操作"]}
            rows={items.map((a, index) => [
              <div className="article-title-cell">
                <div className="article-reorder">
                  <button
                    type="button"
                    title="移到最顶（当前组）"
                    aria-label={`移到最顶 ${a.title}`}
                    disabled={
                      moving ||
                      index === 0 ||
                      !!items[index - 1]?.pinned !== !!a.pinned
                    }
                    onClick={() => void move(a.id, "top")}
                  >
                    <ArrowUpToLine size={14} />
                  </button>
                  <button
                    type="button"
                    title="上移"
                    aria-label={`上移 ${a.title}`}
                    disabled={
                      moving ||
                      index === 0 ||
                      !!items[index - 1]?.pinned !== !!a.pinned
                    }
                    onClick={() => void move(a.id, "up")}
                  >
                    <ArrowUp size={14} />
                  </button>
                  <button
                    type="button"
                    title="下移"
                    aria-label={`下移 ${a.title}`}
                    disabled={
                      moving ||
                      index === items.length - 1 ||
                      !!items[index + 1]?.pinned !== !!a.pinned
                    }
                    onClick={() => void move(a.id, "down")}
                  >
                    <ArrowDown size={14} />
                  </button>
                  <button
                    type="button"
                    title="移到最底（当前组）"
                    aria-label={`移到最底 ${a.title}`}
                    disabled={
                      moving ||
                      index === items.length - 1 ||
                      !!items[index + 1]?.pinned !== !!a.pinned
                    }
                    onClick={() => void move(a.id, "bottom")}
                  >
                    <ArrowDownToLine size={14} />
                  </button>
                </div>
                <strong>{a.title}</strong>
                {a.pinned && (
                  <span
                    className="article-pin"
                    aria-label="已固定到顶部"
                    title="已固定到顶部"
                  >
                    <Pin size={14} fill="currentColor" />
                  </span>
                )}
              </div>,
              <span className="article-category">[{a.category}]</span>,
              <Badge tone={a.published ? "green" : ""}>
                {a.published ? "已发布" : "草稿"}
              </Badge>,
              <Badge tone={a.public && a.published ? "orange" : ""}>
                {a.public
                  ? a.published
                    ? "公开"
                    : "公开（未发布）"
                  : "仅会员"}
              </Badge>,
              date(a.updatedAt),
              <div className="actions">
                <Button
                  disabled={editingBusy}
                  onClick={() => openEditor({ ...a })}
                >
                  编辑
                </Button>
                <Button disabled={moving} onClick={() => void pin(a)}>
                  <Pin size={14} />
                  {a.pinned ? "取消固定" : "固定到顶部"}
                </Button>
                <Button
                  disabled={moving}
                  onClick={() =>
                    void visibility(a, { published: !a.published })
                  }
                >
                  {a.published ? "取消发布" : "发布"}
                </Button>
                <Button
                  disabled={moving}
                  onClick={() => void visibility(a, { public: !a.public })}
                >
                  {a.public ? "取消公开" : "公开到文章页"}
                </Button>
                {a.public && a.published && (
                  <a
                    className="btn"
                    href={`/news/${a.id}`}
                    target="_blank"
                    rel="noopener"
                    aria-label={`查看公开页 ${a.title}`}
                  >
                    查看 <ExternalLink size={13} aria-hidden="true" />
                  </a>
                )}
                <Button onClick={() => void remove(a)}>删除</Button>
              </div>,
            ])}
          />
        </div>
      ) : (
        <div className="article-layout">
          <div className="card article-list">
            {items.map((a) => (
              <button
                key={a.id}
                className={`article-item ${current?.id === a.id ? "active" : ""}`}
                onClick={() => setActive(a.id)}
              >
                <strong>{a.title}</strong>
                <small>
                  {a.pinned && (
                    <Pin
                      className="article-pin-inline"
                      size={12}
                      fill="currentColor"
                      aria-label="已固定到顶部"
                    />
                  )}
                  [{a.category}]
                </small>
              </button>
            ))}
          </div>
          <article className="card article-content">
            {current ? (
              <>
                <div className="between">
                  <div className="actions">
                    {current.pinned && (
                      <span
                        className="article-pin"
                        aria-label="已固定到顶部"
                        title="已固定到顶部"
                      >
                        <Pin size={14} fill="currentColor" />
                      </span>
                    )}
                    <Badge tone="orange">[{current.category}]</Badge>
                  </div>
                  <small>{date(current.updatedAt)}</small>
                </div>
                <h2 className="mt16">{current.title}</h2>
                <Markdown text={current.body} />
                {current.attachments?.length > 0 && (
                  <div className="mt24">
                    <h3>附件下载</h3>
                    {current.attachments.map((f: RecordData) => (
                      <div className="attachment" key={f.id}>
                        <span>
                          {f.name} <small>{Math.ceil(f.size / 1024)} KB</small>
                        </span>
                        <Button
                          onClick={() =>
                            void downloadFile(
                              `/api/articles/${current.id}/attachments/${f.id}`,
                              f.name,
                            ).catch((e) => setMessage(e.message))
                          }
                        >
                          下载
                        </Button>
                      </div>
                    ))}
                  </div>
                )}
              </>
            ) : (
              <Empty>暂无已发布文章</Empty>
            )}
          </article>
        </div>
      )}
      {editing && (
        <Modal
          title={editing.id ? "编辑文章" : "新建文章"}
          onClose={() => {
            setExpanded(false);
            openEditor(null);
          }}
        >
          <fieldset className="article-edit-fields" disabled={editingBusy}>
            <AsyncForm
              label={editing.published ? "发布 / 更新" : "保存草稿"}
              onSubmit={async () => {
                if (editMutation.current) throw new Error("请等待附件操作完成");
                editMutation.current = true;
                setEditingBusy(true);
                const session = editSession.current;
                try {
                  const result = await post(
                    "/api/admin/articles" +
                      (editing.id ? "/" + editing.id : ""),
                    editing,
                    editing.id ? "PUT" : "POST",
                  );
                  if (session === editSession.current) setEditing(result);
                  reload();
                } finally {
                  editMutation.current = false;
                  setEditingBusy(false);
                }
              }}
            >
              <div className="form-grid">
                <Field
                  label="标题"
                  value={editing.title}
                  onChange={(e) =>
                    setEditing({ ...editing, title: e.target.value })
                  }
                  required
                />
                <Select
                  label="分类"
                  value={editing.category}
                  onChange={(e) =>
                    setEditing({ ...editing, category: e.target.value })
                  }
                >
                  <option>教程</option>
                  <option>公告</option>
                  <option>资讯</option>
                  {editing.category &&
                    !["教程", "公告", "资讯"].includes(editing.category) && (
                      <option>{editing.category}</option>
                    )}
                </Select>
              </div>
              <section
                className={`markdown-workspace ${expanded ? "is-expanded" : ""}`}
                aria-label="Markdown 编辑器"
              >
                <div className="markdown-modebar">
                  <div
                    className="markdown-modes"
                    role="tablist"
                    aria-label="编辑模式"
                  >
                    {[
                      ["content", "内容"],
                      ["preview", "预览"],
                      ["compare", "对照"],
                    ].map(([k, t]) => (
                      <button
                        type="button"
                        key={k}
                        role="tab"
                        aria-selected={tab === k}
                        className={tab === k ? "active" : ""}
                        onClick={() => setTab(k)}
                      >
                        {t}
                      </button>
                    ))}
                  </div>
                  <div className="markdown-mode-actions">
                    <small>支持 Markdown · 回车即换行</small>
                    <details className="kbd-help">
                      <summary
                        className="markdown-icon"
                        title="快捷键"
                        aria-label="快捷键"
                      >
                        <Keyboard size={17} />
                      </summary>
                      <div className="kbd-pop">
                        {shortcuts.map(([name, keys]) => (
                          <div key={name}>
                            <span>{name}</span>
                            <kbd>{keys}</kbd>
                          </div>
                        ))}
                      </div>
                    </details>
                    {expanded && (
                      <Button type="submit" className="small" primary>
                        保存
                      </Button>
                    )}
                    <button
                      type="button"
                      className="markdown-icon"
                      title={expanded ? "退出全屏 (Esc)" : "全屏编辑"}
                      aria-label={expanded ? "退出全屏编辑" : "全屏编辑"}
                      onClick={() => setExpanded((value) => !value)}
                    >
                      {expanded ? (
                        <Minimize size={18} />
                      ) : (
                        <Maximize size={18} />
                      )}
                    </button>
                  </div>
                </div>
                <div
                  className="markdown-toolbar"
                  role="toolbar"
                  aria-label="Markdown 格式工具"
                >
                  {formatGroups.map((group, index) => (
                    <Fragment key={index}>
                      {group.map(([name, action, ToolIcon, keys]) => (
                        <button
                          type="button"
                          key={action}
                          title={keys ? `${name} ${keys}` : name}
                          aria-label={name}
                          className="markdown-icon"
                          disabled={!tools || tab === "preview"}
                          onClick={() => tools?.run(action)}
                        >
                          <ToolIcon size={17} />
                        </button>
                      ))}
                      <span className="markdown-divider" />
                    </Fragment>
                  ))}
                  <button
                    type="button"
                    className="markdown-icon"
                    title="撤销 Ctrl+Z"
                    aria-label="撤销"
                    disabled={!tools || !undoable[0]}
                    onClick={() => tools?.run("undo")}
                  >
                    <Undo2 size={17} />
                  </button>
                  <button
                    type="button"
                    className="markdown-icon"
                    title="重做 Ctrl+Y"
                    aria-label="重做"
                    disabled={!tools || !undoable[1]}
                    onClick={() => tools?.run("redo")}
                  >
                    <Redo2 size={17} />
                  </button>
                  <button
                    type="button"
                    className="markdown-icon"
                    title="清空正文"
                    aria-label="清空正文"
                    disabled={!tools || !editing.body}
                    onClick={() => {
                      if (confirm("清空编辑区正文？")) tools?.run("clear");
                    }}
                  >
                    <Eraser size={17} />
                  </button>
                </div>
                <div
                  className={`markdown-panels ${tab === "compare" ? "is-compare" : ""}`}
                >
                  <div
                    className={`markdown-source ${tab === "preview" ? "is-hidden" : ""}`}
                  >
                    <Suspense
                      fallback={
                        <div className="markdown-loading">
                          <LoaderCircle className="spin" size={16} />
                          正在加载编辑器…
                        </div>
                      }
                    >
                      <MarkdownEditor
                        value={editing.body}
                        disabled={editingBusy}
                        onChange={(body) =>
                          setEditing((article) =>
                            article ? { ...article, body } : article,
                          )
                        }
                        onReady={setTools}
                        onHistory={(canUndo, canRedo) =>
                          setUndoable([canUndo, canRedo])
                        }
                        syncScroll={tab === "compare" ? previewPanel : null}
                      />
                    </Suspense>
                  </div>
                  {tab !== "content" && (
                    <div className="markdown-preview" ref={setPreviewPanel}>
                      <Markdown text={editing.body} />
                    </div>
                  )}
                </div>
                <small className="markdown-status">
                  {editing.body.length} 字 · {editing.body.split("\n").length}{" "}
                  行{tab === "compare" && " · 对照模式两侧同步滚动"}
                </small>
              </section>
              <label className="field mt16">
                <span>摘要（可选）</span>
                <textarea
                  rows={2}
                  maxLength={300}
                  value={editing.summary || ""}
                  placeholder="用于文章列表和分享预览；留空则自动截取正文开头"
                  onChange={(e) =>
                    setEditing({ ...editing, summary: e.target.value })
                  }
                />
              </label>
              <div className="form-grid">
                <Check
                  checked={editing.published}
                  onChange={(e) =>
                    setEditing({ ...editing, published: e.target.checked })
                  }
                  label="发布给用户"
                />
                <Check
                  checked={!!editing.public}
                  disabled={!editing.published}
                  onChange={(e) =>
                    setEditing({ ...editing, public: e.target.checked })
                  }
                  label="同时公开到主页和文章页（访客无需登录）"
                />
              </div>
            </AsyncForm>
            <div className="mt16">
              <h3>附件管理</h3>
              {!editing.id && (
                <Notice>
                  选择附件后会自动保存为草稿；只有明确保存发布后才会对用户公开。
                </Notice>
              )}
              <ErrorNotice error={editorMessage} />
              <input
                type="file"
                aria-label="上传文章附件"
                accept=".png,.jpg,.jpeg,.gif,.webp,.pdf,.txt,.md,.zip,.docx"
                onChange={async (e) => {
                  const f = e.target.files?.[0];
                  e.target.value = "";
                  if (!f || editMutation.current) return;
                  editMutation.current = true;
                  setEditingBusy(true);
                  setEditorMessage("");
                  const session = editSession.current;
                  try {
                    let article = editing;
                    if (!article.id) {
                      const draft = await post("/api/admin/articles", {
                        ...article,
                        title: article.title.trim() || "未命名草稿",
                        published: false,
                      });
                      article = {
                        ...article,
                        id: draft.id,
                        published: false,
                        attachments: draft.attachments || [],
                      };
                      // Keep the draft identity even if the subsequent upload fails.
                      if (session === editSession.current) setEditing(article);
                      reload();
                    }
                    const form = new FormData();
                    form.append("file", f);
                    const file = await api(
                      "/api/admin/articles/" + article.id + "/attachments",
                      { method: "POST", body: form },
                    );
                    if (session === editSession.current)
                      setEditing({
                        ...article,
                        attachments: [...(article.attachments || []), file],
                      });
                    reload();
                  } catch (e) {
                    if (session === editSession.current)
                      setEditorMessage((e as Error).message);
                  } finally {
                    editMutation.current = false;
                    setEditingBusy(false);
                  }
                }}
              />
              {editing.id && !editing.published && (
                <small className="article-draft-note">
                  当前为草稿，文章及附件仅管理员可访问。关闭窗口会保留已自动保存的草稿。
                </small>
              )}
              {editing.attachments?.map((f: RecordData) => (
                <div className="attachment" key={f.id}>
                  <span>{f.name}</span>
                  <Button
                    onClick={async () => {
                      if (!confirm("删除这个附件？")) return;
                      if (editMutation.current) return;
                      editMutation.current = true;
                      setEditingBusy(true);
                      setEditorMessage("");
                      const session = editSession.current;
                      try {
                        await api(
                          `/api/admin/articles/${editing.id}/attachments/${f.id}`,
                          { method: "DELETE" },
                        );
                        if (session === editSession.current)
                          setEditing({
                            ...editing,
                            attachments: editing.attachments.filter(
                              (x: RecordData) => x.id !== f.id,
                            ),
                          });
                        reload();
                      } catch (e) {
                        if (session === editSession.current)
                          setEditorMessage((e as Error).message);
                      } finally {
                        editMutation.current = false;
                        setEditingBusy(false);
                      }
                    }}
                  >
                    删除
                  </Button>
                </div>
              ))}
            </div>
          </fieldset>
        </Modal>
      )}
    </>
  );
}
export function Tickets({ admin = false }: { admin?: boolean }) {
  const { data, error, reload } = useData("/api/tickets"),
    [selected, setSelected] = useState<RecordData | null>(null),
    [creating, setCreating] = useState(false),
    [ticketError, setTicketError] = useState("");
  return (
    <>
      <Header title="工单" sub="请说明问题和贴出任务记录">
        {!admin && (
          <Button primary onClick={() => setCreating(true)}>
            新建工单
          </Button>
        )}
      </Header>
      <ErrorNotice error={error || ticketError} />
      <div className="card flush">
        <Table
          headers={[
            "标题",
            "状态",
            "创建时间",
            "最后回复",
            "最后回复时间",
            "操作",
          ]}
          rows={array(data, "tickets").map((t) => [
            t.title,
            t.status === "open" ? "处理中" : "已关闭",
            date(t.createdAt),
            <Badge tone={t.lastReplyRole === "admin" ? "orange" : ""}>
              {t.lastReplyRole === "admin"
                ? "管理员"
                : t.lastReplyRole === "user"
                  ? "用户"
                  : "暂无回复"}
              {t.replyCount === 0 && t.lastReplyRole ? " · 新建" : ""}
            </Badge>,
            date(t.lastReplyAt),
            <Button
              onClick={async () => {
                setSelected(t);
                setTicketError("");
                try {
                  await post(`/api/tickets/${t.id}/read`, {});
                  reload();
                  window.dispatchEvent(new Event("msboost:tickets-changed"));
                } catch (e) {
                  setTicketError((e as Error).message);
                }
              }}
            >
              查看 / 回复
            </Button>,
          ])}
        />
      </div>
      {creating && (
        <Modal title="新建工单" onClose={() => setCreating(false)}>
          <AsyncForm
            label="提交工单"
            onSubmit={(f) =>
              post("/api/tickets", {
                title: f.get("title"),
                body: f.get("body"),
              })
            }
            onDone={() => {
              setCreating(false);
              reload();
            }}
          >
            <Field label="标题" name="title" required />
            <label className="field">
              <span>问题描述</span>
              <textarea name="body" rows={6} required />
            </label>
          </AsyncForm>
        </Modal>
      )}
      {selected && (
        <Modal title={selected.title} onClose={() => setSelected(null)}>
          <div className="ticket-metadata">
            <span>创建：{date(selected.createdAt)}</span>
            <span>
              最后回复：
              {selected.lastReplyRole === "admin"
                ? "管理员"
                : selected.lastReplyRole === "user"
                  ? "用户"
                  : "暂无回复"}{" "}
              · {date(selected.lastReplyAt)}
            </span>
          </div>
          {selected.replies.map((v: RecordData) => (
            <div className="card mt16" key={v.id}>
              <div className="between">
                <strong>
                  {v.admin ? "管理员" : "用户"}
                  <small>{v.author}</small>
                </strong>
                <small>{date(v.createdAt)}</small>
              </div>
              <p style={{ whiteSpace: "pre-wrap" }} className="mt8">
                {v.body}
              </p>
            </div>
          ))}
          {selected.status === "open" && (
            <AsyncForm
              label="发送回复"
              onSubmit={(f) =>
                post("/api/tickets/" + selected.id + "/replies", {
                  body: f.get("body"),
                })
              }
              onDone={() => {
                setSelected(null);
                reload();
              }}
            >
              <label className="field mt16">
                <span>回复内容</span>
                <textarea name="body" required rows={4} />
              </label>
            </AsyncForm>
          )}
          <Button
            className="mt16"
            onClick={async () => {
              await post(
                "/api/tickets/" + selected.id,
                { status: selected.status === "open" ? "closed" : "open" },
                "PATCH",
              );
              setSelected(null);
              reload();
            }}
          >
            {selected.status === "open" ? "关闭工单" : "重新打开"}
          </Button>
        </Modal>
      )}
    </>
  );
}
