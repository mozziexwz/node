import { useEffect, useState } from "react";
import { Calendar, Save } from "lucide-react";
import { post } from "./api";
import {
  Badge,
  Button,
  ErrorNotice,
  Field,
  Markdown,
  Notice,
  date,
  useData,
} from "./ui";

// What changed in the node installer. The administrator writes it in
// 系统设置 → 升级节点; the deploy page's upgrade mode shows it above its form
// so members can decide whether to upgrade. Empty notes hide the card.
export type NodeUpgrade = {
  releasedAt?: string;
  notes?: string;
  updatedAt?: number;
};

export function NodeUpgradeNotes({ upgrade }: { upgrade?: NodeUpgrade }) {
  if (!upgrade?.notes) return null;
  return (
    <div className="card">
      <div className="launcher-head">
        <h3>节点更新内容</h3>
        {upgrade.releasedAt && (
          <Badge>
            <Calendar size={12} />
            更新于 {upgrade.releasedAt}
          </Badge>
        )}
      </div>
      <div className="launcher-notes">
        <Markdown text={upgrade.notes} />
      </div>
    </div>
  );
}

export function NodeUpgradeSettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, loading, reload } = useData("/api/admin/settings");
  const [values, setValues] = useState<NodeUpgrade | null>(null),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  useEffect(() => {
    if (data) setValues({ ...(data.settings || data).nodeUpgrade });
  }, [data]);
  if (loading && !values) return <Notice>正在读取升级节点设置…</Notice>;
  if (!values) return <ErrorNotice error={error || "升级节点设置读取失败"} />;
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setFailure("");
        setMessage("");
        try {
          await post(
            "/api/admin/settings",
            {
              nodeUpgrade: {
                releasedAt: values.releasedAt || "",
                notes: values.notes || "",
              },
            },
            "PUT",
          );
          setMessage("升级节点设置已保存");
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
        这里填写的内容显示在「部署 MSBOOST →
        升级节点」页面最上方的「节点更新内容」，告诉会员新版节点改了什么。网站发布新版节点后改这里即可；更新内容留空时不显示这张卡。
      </Notice>
      <div className="form-grid mt24">
        <Field
          label="更新日期"
          type="date"
          value={values.releasedAt || ""}
          onChange={(e) => setValues({ ...values, releasedAt: e.target.value })}
        />
        <label className="field span-all">
          <span>更新内容（可选，支持 Markdown）</span>
          <textarea
            className="notes-input"
            maxLength={5000}
            value={values.notes || ""}
            onChange={(e) => setValues({ ...values, notes: e.target.value })}
          />
          <small>
            显示在升级页面的「节点更新内容」里，最多 5000 字；留空则不显示
          </small>
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
        <Button type="submit" primary disabled={busy}>
          <Save size={15} />
          {busy ? "正在保存…" : "保存"}
        </Button>
      </div>
    </form>
  );
}
