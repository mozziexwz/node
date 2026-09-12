import { useEffect, useRef, useState } from "react";
import {
  ArrowUp,
  ArrowDown,
  Bold,
  Italic,
  Strikethrough,
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
import "./content-extra.css";
export function Articles({ admin = false }: { admin?: boolean }) {
  const { data, error, reload } = useData(
      admin ? "/api/admin/articles" : "/api/articles",
    ),
    [active, setActive] = useState(""),
    [editing, setEditing] = useState<RecordData | null>(null),
    [tab, setTab] = useState("content"),
    [message, setMessage] = useState(""),
    [expanded, setExpanded] = useState(false),
    [moving, setMoving] = useState(false);
  const editor = useRef<HTMLTextAreaElement>(null),
    history = useRef<string[]>([]),
    redo = useRef<string[]>([]);
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
  function change(text: string) {
    if (!editing) return;
    history.current.push(editing.body);
    redo.current = [];
    setEditing({ ...editing, body: text });
  }
  function insert(before: string, after = "") {
    if (!editor.current || !editing) return;
    const el = editor.current,
      s = el.selectionStart,
      e = el.selectionEnd;
    change(
      editing.body.slice(0, s) +
        before +
        editing.body.slice(s, e) +
        after +
        editing.body.slice(e),
    );
    requestAnimationFrame(() => {
      el.focus();
      el.setSelectionRange(s + before.length, e + before.length);
    });
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
  async function move(id: string, direction: "up" | "down") {
    if (moving) return;
    setMoving(true);
    setMessage("");
    try {
      await post(`/api/admin/articles/${id}/move`, { direction });
      reload();
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
          admin ? "编辑正文、附件与发布状态。" : "先了解操作流程，再开始部署。"
        }
      >
        {admin && (
          <Button
            primary
            onClick={() => {
              setEditing({
                title: "",
                body: "",
                category: "教程",
                published: false,
                sort: 0,
              });
              history.current = [];
              redo.current = [];
            }}
          >
            新建文章
          </Button>
        )}
      </Header>
      <ErrorNotice error={error || message} />
      {admin ? (
        <div className="card flush">
          <Table
            headers={["标题", "分类", "状态", "更新", "操作"]}
            rows={items.map((a, index) => [
              <div className="article-title-cell">
                <div className="article-reorder">
                  <button
                    type="button"
                    title="上移"
                    aria-label={`上移 ${a.title}`}
                    disabled={moving || index === 0}
                    onClick={() => void move(a.id, "up")}
                  >
                    <ArrowUp size={14} />
                  </button>
                  <button
                    type="button"
                    title="下移"
                    aria-label={`下移 ${a.title}`}
                    disabled={moving || index === items.length - 1}
                    onClick={() => void move(a.id, "down")}
                  >
                    <ArrowDown size={14} />
                  </button>
                </div>
                <strong>{a.title}</strong>
              </div>,
              a.category,
              <Badge tone={a.published ? "green" : ""}>
                {a.published ? "已发布" : "草稿"}
              </Badge>,
              date(a.updatedAt),
              <div className="actions">
                <Button
                  onClick={() => {
                    setEditing({ ...a });
                    history.current = [];
                    redo.current = [];
                  }}
                >
                  编辑
                </Button>
                <Button
                  onClick={async () => {
                    try {
                      await post(
                        "/api/admin/articles/" + a.id,
                        { ...a, published: !a.published },
                        "PUT",
                      );
                      reload();
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  {a.published ? "取消发布" : "发布"}
                </Button>
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
                <small>{a.category}</small>
              </button>
            ))}
          </div>
          <article className="card article-content">
            {current ? (
              <>
                <div className="between">
                  <Badge tone="orange">{current.category}</Badge>
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
            setEditing(null);
          }}
        >
          <AsyncForm
            label={editing.published ? "发布 / 更新" : "保存草稿"}
            onSubmit={async () => {
              const result = await post(
                "/api/admin/articles" + (editing.id ? "/" + editing.id : ""),
                editing,
                editing.id ? "PUT" : "POST",
              );
              setEditing(result);
              reload();
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
                  <small>支持 Markdown</small>
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
                    {expanded ? <Minimize size={18} /> : <Maximize size={18} />}
                  </button>
                </div>
              </div>
              <div
                className="markdown-toolbar"
                role="toolbar"
                aria-label="Markdown 格式工具"
              >
                {[
                  ["加粗", "**", "**", Bold],
                  ["斜体", "_", "_", Italic],
                  ["删除线", "~~", "~~", Strikethrough],
                  ["标题", "\n## ", "", Heading],
                  ["无序列表", "\n- ", "", List],
                  ["有序列表", "\n1. ", "", ListOrdered],
                  ["引用", "\n> ", "", Quote],
                  ["链接", "[", "](https://)", Link],
                  ["图片", "![", "](https://)", Image],
                  ["代码", "\n```\n", "\n```", Code],
                  [
                    "表格",
                    "\n| 列1 | 列2 |\n| --- | --- |\n| 内容 | 内容 |\n",
                    "",
                    Table2,
                  ],
                  ["分隔线", "\n\n---\n\n", "", Minus],
                ].map(([t, b, a, Icon]) => {
                  const ToolIcon = Icon as typeof Bold;
                  return (
                    <button
                      type="button"
                      key={String(t)}
                      title={String(t)}
                      aria-label={String(t)}
                      className="markdown-icon"
                      disabled={tab === "preview"}
                      onClick={() => insert(String(b), String(a))}
                    >
                      <ToolIcon size={17} />
                    </button>
                  );
                })}
                <span className="markdown-divider" />
                <button
                  type="button"
                  className="markdown-icon"
                  title="撤销"
                  aria-label="撤销"
                  disabled={!history.current.length}
                  onClick={() => {
                    const t = history.current.pop();
                    if (t !== undefined) {
                      redo.current.push(editing.body);
                      setEditing({ ...editing, body: t });
                    }
                  }}
                >
                  <Undo2 size={17} />
                </button>
                <button
                  type="button"
                  className="markdown-icon"
                  title="重做"
                  aria-label="重做"
                  disabled={!redo.current.length}
                  onClick={() => {
                    const t = redo.current.pop();
                    if (t !== undefined) {
                      history.current.push(editing.body);
                      setEditing({ ...editing, body: t });
                    }
                  }}
                >
                  <Redo2 size={17} />
                </button>
                <button
                  type="button"
                  className="markdown-icon"
                  title="清空正文"
                  aria-label="清空正文"
                  onClick={() => {
                    if (confirm("清空编辑区正文？")) change("");
                  }}
                >
                  <Eraser size={17} />
                </button>
              </div>
              <div
                className={`markdown-panels ${tab === "compare" ? "is-compare" : ""}`}
              >
                {tab !== "preview" && (
                  <textarea
                    className="markdown-source"
                    aria-label="文章 Markdown 正文"
                    ref={editor}
                    value={editing.body}
                    onChange={(e) => change(e.target.value)}
                  />
                )}{" "}
                {tab !== "content" && (
                  <div className="markdown-preview">
                    <Markdown text={editing.body} />
                  </div>
                )}
              </div>
              <small className="markdown-status">
                {editing.body.length} 字 · {editing.body.split("\n").length} 行
              </small>
            </section>
            <div className="form-grid mt16">
              <Check
                checked={editing.published}
                onChange={(e) =>
                  setEditing({ ...editing, published: e.target.checked })
                }
                label="发布给用户"
              />
            </div>
          </AsyncForm>
          {editing.id && (
            <div className="mt16">
              <h3>附件管理</h3>
              <input
                type="file"
                aria-label="上传文章附件"
                accept=".png,.jpg,.jpeg,.gif,.webp,.pdf,.txt,.md,.zip,.docx"
                onChange={async (e) => {
                  const f = e.target.files?.[0];
                  if (!f) return;
                  try {
                    const form = new FormData();
                    form.append("file", f);
                    const file = await api(
                      "/api/admin/articles/" + editing.id + "/attachments",
                      { method: "POST", body: form },
                    );
                    setEditing({
                      ...editing,
                      attachments: [...(editing.attachments || []), file],
                    });
                    reload();
                  } catch (e) {
                    setMessage((e as Error).message);
                  }
                }}
              />
              {editing.attachments?.map((f: RecordData) => (
                <div className="attachment" key={f.id}>
                  <span>{f.name}</span>
                  <Button
                    onClick={async () => {
                      if (!confirm("删除这个附件？")) return;
                      await api(
                        `/api/admin/articles/${editing.id}/attachments/${f.id}`,
                        { method: "DELETE" },
                      );
                      setEditing({
                        ...editing,
                        attachments: editing.attachments.filter(
                          (x: RecordData) => x.id !== f.id,
                        ),
                      });
                      reload();
                    }}
                  >
                    删除
                  </Button>
                </div>
              ))}
            </div>
          )}
        </Modal>
      )}
    </>
  );
}
export function Tickets() {
  const { data, error, reload } = useData("/api/tickets"),
    [selected, setSelected] = useState<RecordData | null>(null),
    [creating, setCreating] = useState(false);
  return (
    <>
      <Header
        title="工单"
        sub="请说明问题和任务编号，不要填写 SSH 密码或配置认证信息。"
      >
        <Button primary onClick={() => setCreating(true)}>
          新建工单
        </Button>
      </Header>
      <ErrorNotice error={error} />
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
            <Button onClick={() => setSelected(t)}>查看 / 回复</Button>,
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
