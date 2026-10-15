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

// Feature cards are edited as "标题 | 说明" lines, which keeps the form compact
// while the server still validates every field.
const featuresText = (features: RecordData[] = []) =>
  features.map((f) => (f.desc ? `${f.title} | ${f.desc}` : f.title)).join("\n");
const parseFeatures = (value: string) =>
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
    [features, setFeatures] = useState("");
  function load(source: RecordData) {
    setHome({ ...source.home });
    setSeo({ ...source.seo });
    setPoints((source.home.points || []).join("\n"));
    setFeatures(featuresText(source.home.features));
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
          hint="每行一张，格式「标题 | 说明」，最多 6 张"
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
          label="主页显示最新公开文章"
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
