import React, { useEffect, useState } from "react";
import { ExternalLink } from "lucide-react";
import { post, RecordData } from "./api";
import {
  AsyncForm,
  Button,
  Check,
  ErrorNotice,
  Field,
  Notice,
  Select,
  useData,
} from "./ui";

const lines = (value: string) =>
  value
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);

// Feature cards are edited as "标题 | 说明 | 跳转" lines, which keeps the form
// compact while the server still validates every field. The optional last part
// is recognised by its form: a #section, a site path or an https link.
const isTarget = (value: string) => /^(#|\/|https:\/\/)/.test(value);
const featuresText = (features: RecordData[] = []) =>
  features
    .map((f) =>
      [f.title, f.desc || "", f.target || ""]
        .join(" | ")
        .replace(/( \| )+$/, ""),
    )
    .join("\n");
const parseFeatures = (value: string) =>
  lines(value).map((line) => {
    const [title, ...rest] = line.split("|").map((part) => part.trim());
    const target =
      rest.length > 1 && isTarget(rest[rest.length - 1])
        ? rest.pop() || ""
        : "";
    return { title, desc: rest.join(" | "), target };
  });
// Diagram boxes and points of the homepage explanations are "标题 | 说明" lines.
const itemsText = (items: RecordData[] = []) =>
  items.map((i) => (i.desc ? `${i.title} | ${i.desc}` : i.title)).join("\n");
const parseItems = (value: string) =>
  lines(value).map((line) => {
    const [title, ...rest] = line.split("|");
    return { title: title.trim(), desc: rest.join("|").trim() };
  });

function TextArea({
  label,
  hint,
  ...props
}: React.TextareaHTMLAttributes<HTMLTextAreaElement> & {
  label: string;
  hint?: string;
}) {
  return (
    <label className="field">
      <span>{label}</span>
      <textarea {...props} />
      {hint && <small>{hint}</small>}
    </label>
  );
}

export function SiteSettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, reload } = useData("/api/admin/site");
  const [home, setHome] = useState<RecordData | null>(null),
    [seo, setSeo] = useState<RecordData | null>(null),
    [points, setPoints] = useState(""),
    [features, setFeatures] = useState(""),
    [lists, setLists] = useState<Record<string, string>>({});
  // Explanation lists are edited as text; the rest of each section lives in home.
  function loadExplain(source: RecordData) {
    const msboost = source.msboost || {},
      relay = source.relay || {};
    setLists({
      msboostNodes: itemsText(msboost.nodes),
      msboostPoints: itemsText(msboost.points),
      relayDirect: itemsText(relay.direct),
      relayVia: itemsText(relay.via),
      relayPoints: itemsText(relay.points),
    });
  }
  function load(source: RecordData) {
    setHome({ ...source.home });
    setSeo({ ...source.seo });
    setPoints((source.home.points || []).join("\n"));
    setFeatures(featuresText(source.home.features));
    loadExplain(source.home);
  }
  useEffect(() => {
    if (data) load(data);
  }, [data]);
  if (!data || !home || !seo)
    return error ? (
      <>
        <ErrorNotice error={error} />
        <Button className="mt16" onClick={reload}>
          重新读取
        </Button>
      </>
    ) : (
      <Notice>正在读取主页与 SEO 设置…</Notice>
    );
  const setH = (key: string, value: unknown) =>
    setHome({ ...home, [key]: value });
  const setS = (key: string, value: unknown) =>
    setSeo({ ...seo, [key]: value });
  const setSection = (section: string, key: string, value: string) =>
    setHome({ ...home, [section]: { ...home[section], [key]: value } });
  const setList = (key: string, value: string) =>
    setLists({ ...lists, [key]: value });
  const sectionField = (
    section: string,
    key: string,
    label: string,
    max: number,
  ) => (
    <Field
      label={label}
      value={home[section]?.[key] || ""}
      maxLength={max}
      onChange={(e) => setSection(section, key, e.target.value)}
    />
  );
  const sectionText = (
    section: string,
    key: string,
    label: string,
    max: number,
    rows = 3,
  ) => (
    <TextArea
      label={label}
      rows={rows}
      maxLength={max}
      value={home[section]?.[key] || ""}
      onChange={(e) => setSection(section, key, e.target.value)}
    />
  );
  const listText = (key: string, label: string, rows: number) => (
    <TextArea
      label={label}
      rows={rows}
      value={lists[key] || ""}
      onChange={(e) => setList(key, e.target.value)}
    />
  );
  return (
    <AsyncForm
      label="保存主页与 SEO"
      onSubmit={async () => {
        const result = await post(
          "/api/admin/site",
          {
            home: {
              ...home,
              points: lines(points),
              features: parseFeatures(features),
              msboost: {
                ...home.msboost,
                nodes: parseItems(lists.msboostNodes || ""),
                points: parseItems(lists.msboostPoints || ""),
              },
              relay: {
                ...home.relay,
                direct: parseItems(lists.relayDirect || ""),
                via: parseItems(lists.relayVia || ""),
                points: parseItems(lists.relayPoints || ""),
              },
              latestCount: Number(home.latestCount) || 3,
            },
            seo,
          },
          "PUT",
        );
        load(result);
      }}
      onDone={onSaved}
    >
      <div className="between">
        <h3>主页 /home</h3>
        <a className="btn" href="/home" target="_blank" rel="noopener">
          预览主页 <ExternalLink size={13} aria-hidden="true" />
        </a>
      </div>
      <p className="muted mt8">
        访客无需登录即可访问。保存后立即生效；留空的按钮和横幅不会显示。
      </p>
      <div className="form-grid mt16">
        <Field
          label="小标题"
          value={home.eyebrow}
          maxLength={60}
          onChange={(e) => setH("eyebrow", e.target.value)}
        />
        <Field
          label="联系邮箱（页脚）"
          type="email"
          value={home.contactEmail}
          maxLength={120}
          onChange={(e) => setH("contactEmail", e.target.value)}
        />
        <Field
          label="主标题第一行"
          value={home.titleLine1}
          maxLength={40}
          required
          onChange={(e) => setH("titleLine1", e.target.value)}
        />
        <Field
          label="主标题第二行（橙色）"
          value={home.titleLine2}
          maxLength={40}
          onChange={(e) => setH("titleLine2", e.target.value)}
        />
      </div>
      <TextArea
        label="副标题"
        rows={3}
        maxLength={300}
        value={home.subtitle}
        hint="可换行，最多 300 字"
        onChange={(e) => setH("subtitle", e.target.value)}
      />
      <div className="form-grid">
        <TextArea
          label="要点标签"
          rows={4}
          value={points}
          hint="每行一项，最多 4 项，每项不超过 20 字"
          onChange={(e) => setPoints(e.target.value)}
        />
        <TextArea
          label="功能卡片"
          rows={4}
          value={features}
          hint="每行一张，格式「标题 | 说明 | 点击后跳到」，最多 6 张。第三段可选：#msboost（MSBOOST 说明）、#relay（中转服务器说明）、#launcher（启动器下载），或以 / 开头的站内路径、https:// 链接；不填则卡片不可点击"
          onChange={(e) => setFeatures(e.target.value)}
        />
        <Field
          label="主按钮文字"
          value={home.primaryCta?.text}
          maxLength={12}
          onChange={(e) =>
            setH("primaryCta", { ...home.primaryCta, text: e.target.value })
          }
        />
        <Field
          label="主按钮链接"
          value={home.primaryCta?.href}
          placeholder="/ 或 https://"
          onChange={(e) =>
            setH("primaryCta", { ...home.primaryCta, href: e.target.value })
          }
        />
        <Field
          label="次按钮文字"
          value={home.secondaryCta?.text}
          maxLength={12}
          onChange={(e) =>
            setH("secondaryCta", { ...home.secondaryCta, text: e.target.value })
          }
        />
        <Field
          label="次按钮链接"
          value={home.secondaryCta?.href}
          placeholder="/news 或 https://"
          onChange={(e) =>
            setH("secondaryCta", { ...home.secondaryCta, href: e.target.value })
          }
        />
        <Field
          label="公告横幅（可选）"
          value={home.notice}
          maxLength={120}
          onChange={(e) => setH("notice", e.target.value)}
        />
        <Field
          label="公告横幅链接（可选）"
          value={home.noticeHref}
          placeholder="/news/… 或 https://"
          onChange={(e) => setH("noticeHref", e.target.value)}
        />
      </div>
      <div className="form-grid">
        <Check
          label="主页显示「什么是 MSBOOST」「什么是中转服务器」"
          checked={!!home.showExplain}
          onChange={(e) => setH("showExplain", e.target.checked)}
        />
        <Check
          label="主页显示「下载 MSBOOST 启动器」"
          checked={!!home.showLauncher}
          onChange={(e) => setH("showLauncher", e.target.checked)}
        />
        <Check
          label="主页显示最新公开文章（排在最底部）"
          checked={!!home.showLatest}
          onChange={(e) => setH("showLatest", e.target.checked)}
        />
        <Select
          label="最新文章数量"
          value={String(home.latestCount || 3)}
          disabled={!home.showLatest}
          onChange={(e) => setH("latestCount", Number(e.target.value))}
        >
          <option value="3">3 篇</option>
          <option value="6">6 篇</option>
        </Select>
      </div>
      <h3 className="mt24">主页说明</h3>
      <p className="muted mt8">
        「什么是
        MSBOOST」「什么是中转服务器」两段的文字。引言、常见误解和实际上可以用
        **加粗**；示意图和要点每行一项，格式「标题 |
        说明」。按钮和示意图的颜色固定。
      </p>
      <div className="explain-editor mt16">
        <div className="explain-editor-col">
          <h4>什么是 MSBOOST</h4>
          <div className="form-grid">
            {sectionField("msboost", "eyebrow", "小标题", 20)}
            {sectionField("msboost", "title", "标题", 30)}
          </div>
          {sectionText("msboost", "lead", "引言", 300, 4)}
          {listText("msboostNodes", "示意图（3 个方框）", 3)}
          {listText("msboostPoints", "要点（1–3 条）", 5)}
        </div>
        <div className="explain-editor-col">
          <h4>什么是中转服务器</h4>
          <div className="form-grid">
            {sectionField("relay", "eyebrow", "小标题", 20)}
            {sectionField("relay", "title", "标题", 30)}
          </div>
          {sectionText("relay", "lead", "引言", 300, 4)}
          {listText("relayDirect", "示意图 · 直连（3 个方框）", 3)}
          {listText("relayVia", "示意图 · 经过中转（4 个方框）", 4)}
          <div className="form-grid">
            {sectionText("relay", "myth", "常见误解", 200)}
            {sectionText("relay", "truth", "实际上", 200)}
          </div>
          {listText("relayPoints", "要点（1–3 条）", 5)}
        </div>
      </div>
      <div className="actions mt8">
        <Button
          onClick={() => {
            const defaults = data.defaults.home;
            setHome({
              ...home,
              msboost: defaults.msboost,
              relay: defaults.relay,
            });
            loadExplain(defaults);
          }}
        >
          恢复说明的默认文案
        </Button>
      </div>
      <h3 className="mt24">SEO 与分享预览</h3>
      <p className="muted mt8">
        用于 /home、/news
        和每篇公开文章的页面标题、描述和分享卡片。文章页标题自动使用「文章标题 ·
        站点名称」，描述优先使用文章摘要。
      </p>
      <div className="form-grid mt16">
        <Field
          label="站点名称"
          value={seo.siteName}
          maxLength={40}
          required
          onChange={(e) => setS("siteName", e.target.value)}
        />
        <Field
          label="关键词（可选）"
          value={seo.keywords}
          maxLength={200}
          placeholder="用逗号分隔"
          onChange={(e) => setS("keywords", e.target.value)}
        />
        <Field
          label="主页标题"
          value={seo.homeTitle}
          maxLength={80}
          required
          onChange={(e) => setS("homeTitle", e.target.value)}
        />
        <Field
          label="文章页标题"
          value={seo.newsTitle}
          maxLength={80}
          required
          onChange={(e) => setS("newsTitle", e.target.value)}
        />
      </div>
      <TextArea
        label="站点描述"
        rows={2}
        maxLength={200}
        value={seo.description}
        hint="搜索结果和分享卡片中显示，建议 60–120 字"
        onChange={(e) => setS("description", e.target.value)}
      />
      <Field
        label="分享图片（可选）"
        value={seo.ogImage}
        placeholder="https://… 或 /…"
        hint="建议 1200×630 的 PNG 或 JPG；站内路径需要已配置的 PUBLIC_URL"
        onChange={(e) => setS("ogImage", e.target.value)}
      />
      <Check
        label="允许搜索引擎收录主页和公开文章"
        checked={!!seo.allowIndexing}
        onChange={(e) => setS("allowIndexing", e.target.checked)}
      />
      <div className="actions mt8">
        <Button onClick={() => load(data.defaults)}>恢复默认文案</Button>
      </div>
      <small className="muted">恢复默认后仍需点击保存才会生效。</small>
    </AsyncForm>
  );
}
