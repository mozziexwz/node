import React, { useEffect, useMemo, useRef, useState } from "react";
import { ChartLine, Info, Table2 } from "lucide-react";
import { post, RecordData } from "./api";
import {
  Header,
  Button,
  Badge,
  Notice,
  Empty,
  Loading,
  Field,
  Check,
  ErrorNotice,
  useData,
} from "./ui";
import {
  ProbePoint,
  ProbeRoute,
  ProbeSample,
  bucketMinutes,
  failingSince,
  linePoints,
  lineRuns,
  niceScale,
  probeStats,
  probeStatus,
  probeStep,
  rateText,
  statusSegments,
  tableRows,
  visibleSamples,
  xTicks,
} from "./probe-model";

const pad = (n: number) => String(n).padStart(2, "0");
const hm = (t: number) => {
  const d = new Date(t);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
const md = (t: number) => {
  const d = new Date(t);
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
};
const zoneOf = (t: number) => new Date(t).getTimezoneOffset() * 60000;
const ms = (value: number | null) => (value === null ? "—" : `${value} ms`);

// Plot geometry, in CSS pixels.
const L = 46,
  R = 10,
  T = 10,
  PH = 112,
  GAP = 10,
  TH = 12,
  XB = 22,
  H = T + PH + GAP + TH + XB,
  BASE = T + PH,
  MID = BASE + GAP + TH / 2;

function ProbeChart({
  samples,
  hours,
  now,
  step,
  label,
}: {
  samples: ProbeSample[];
  hours: number;
  now: number;
  step: number;
  label: string;
}) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  // index -1 is a moment no probe covers.
  const [hover, setHover] = useState<{ t: number; index: number } | null>(null);
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const measure = () => setWidth(Math.floor(el.clientWidth));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  const from = now - hours * 3600000;
  const points = useMemo(
    () => linePoints(samples, hours, zoneOf(now)),
    [samples, hours, now],
  );
  const reach = Math.max(step, bucketMinutes(hours) * 60000);
  const runs = useMemo(() => lineRuns(points, 2.5 * reach), [points, reach]);
  const segments = useMemo(
    () => statusSegments(samples, step, from, now),
    [samples, step, from, now],
  );
  const W = Math.max(240, width);
  const x = (t: number) => L + ((t - from) / (now - from)) * (W - L - R);
  const scale = niceScale(
    points.reduce((max, p) => Math.max(max, p.ms ?? 0), 0) || 100,
  );
  const y = (value: number) => BASE - (value / scale.top) * PH;
  const grid: number[] = [];
  for (let v = 0; v <= scale.top; v += scale.step) grid.push(v);
  const lastSample = samples[samples.length - 1];
  const lastPoint = points[points.length - 1];
  const live =
    lastSample &&
    lastSample[1] >= 0 &&
    now - lastSample[0] <= 2.5 * step &&
    lastPoint?.ms != null;
  const show = (index: number) => {
    if (!points.length) return;
    const i = Math.max(0, Math.min(points.length - 1, index));
    setHover({ t: points[i].t, index: i });
  };
  const shown: ProbePoint | null =
    hover && hover.index >= 0 ? points[hover.index] || null : null;
  const hoverX = hover ? x(hover.t) : 0;
  return (
    <div
      className="probe-chart"
      ref={box}
      tabIndex={0}
      role="img"
      aria-label={label}
      onPointerMove={(e) => {
        const svg = box.current?.querySelector("svg");
        if (!svg || !points.length) return;
        const px = e.clientX - svg.getBoundingClientRect().left;
        const t = Math.max(
          from,
          Math.min(now, from + ((px - L) / (W - L - R)) * (now - from)),
        );
        let best = 0;
        points.forEach((p, i) => {
          if (Math.abs(p.t - t) < Math.abs(points[best].t - t)) best = i;
        });
        setHover(
          Math.abs(points[best].t - t) > reach
            ? { t, index: -1 }
            : { t: points[best].t, index: best },
        );
      }}
      onPointerLeave={() => setHover(null)}
      onBlur={() => setHover(null)}
      onFocus={(e) => {
        if (e.currentTarget.matches(":focus-visible")) show(points.length - 1);
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") return setHover(null);
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault();
        show(
          (hover && hover.index >= 0 ? hover.index : points.length - 1) +
            (e.key === "ArrowLeft" ? -1 : 1),
        );
      }}
    >
      {width > 0 && (
        <svg width={W} height={H} viewBox={`0 0 ${W} ${H}`} aria-hidden="true">
          {grid.map((v) => {
            const gy = Math.round(y(v)) + 0.5;
            return (
              <g key={v}>
                <line
                  x1={L}
                  x2={W - R}
                  y1={gy}
                  y2={gy}
                  strokeWidth={1}
                  style={{ stroke: v ? "var(--line)" : "var(--probe-axis)" }}
                />
                <text
                  x={L - 8}
                  y={gy}
                  textAnchor="end"
                  dominantBaseline="middle"
                >
                  {v === scale.top ? `${v} ms` : v}
                </text>
              </g>
            );
          })}
          {runs.map((run) => {
            const key = run[0].t;
            if (run.length === 1)
              return (
                <circle
                  key={key}
                  cx={x(run[0].t)}
                  cy={y(run[0].ms!)}
                  r={1.5}
                  style={{ fill: "var(--probe-line)" }}
                />
              );
            const d = run
              .map(
                (p, i) =>
                  `${i ? "L" : "M"}${x(p.t).toFixed(1)} ${y(p.ms!).toFixed(1)}`,
              )
              .join("");
            return (
              <g key={key}>
                <path
                  d={`${d}L${x(run[run.length - 1].t).toFixed(1)} ${BASE}L${x(run[0].t).toFixed(1)} ${BASE}Z`}
                  style={{ fill: "var(--probe-wash)" }}
                />
                <path
                  d={d}
                  fill="none"
                  strokeWidth={2}
                  strokeLinejoin="round"
                  strokeLinecap="round"
                  style={{ stroke: "var(--probe-line)" }}
                />
              </g>
            );
          })}
          {live && (
            <circle
              cx={x(lastPoint.t)}
              cy={y(lastPoint.ms!)}
              r={4}
              strokeWidth={2}
              style={{ fill: "var(--probe-line)", stroke: "#fff" }}
            />
          )}
          {/* Every probe's result on the same time axis. Height repeats the
              colour: failed is tallest, no data is a hairline. */}
          {segments.map((g, i) => {
            if (g.status === "fail") return null;
            const xa = x(g.a) + (i ? 1 : 0),
              xb = x(g.b) - (i < segments.length - 1 ? 1 : 0),
              tall = g.status === "ok" ? 6 : 2;
            if (xb - xa < 0.5) return null;
            return (
              <rect
                key={g.a}
                x={xa}
                y={MID - tall / 2}
                width={xb - xa}
                height={tall}
                rx={tall / 3}
                style={{
                  fill:
                    g.status === "ok" ? "var(--probe-ok)" : "var(--probe-none)",
                }}
              />
            );
          })}
          {segments.map((g) => {
            if (g.status !== "fail") return null;
            const wide = Math.max(4, x(g.b) - x(g.a));
            const center = Math.max(
              L + wide / 2,
              Math.min(W - R - wide / 2, (x(g.a) + x(g.b)) / 2),
            );
            return (
              <rect
                key={g.a}
                x={center - wide / 2}
                y={MID - TH / 2}
                width={wide}
                height={TH}
                rx={2}
                strokeWidth={4}
                style={{
                  fill: "var(--probe-fail)",
                  stroke: "#fff",
                  paintOrder: "stroke",
                }}
              />
            );
          })}
          {xTicks(from, now, hours).map((t) => {
            const tx = x(t);
            return (
              <g key={t}>
                <line
                  x1={tx}
                  x2={tx}
                  y1={BASE}
                  y2={BASE + 4}
                  strokeWidth={1}
                  style={{ stroke: "var(--probe-axis)" }}
                />
                <text
                  x={tx}
                  y={H - 6}
                  textAnchor={
                    tx - L < 18 ? "start" : W - R - tx < 18 ? "end" : "middle"
                  }
                >
                  {hm(t)}
                </text>
              </g>
            );
          })}
          {hover && (
            <line
              x1={hoverX}
              x2={hoverX}
              y1={T}
              y2={MID + TH / 2}
              strokeWidth={1}
              style={{ stroke: "var(--probe-cross)" }}
            />
          )}
          {shown && shown.ms !== null && (
            <circle
              cx={hoverX}
              cy={y(shown.ms)}
              r={4}
              strokeWidth={2}
              style={{ fill: "var(--probe-line)", stroke: "#fff" }}
            />
          )}
        </svg>
      )}
      {hover && (
        <div
          className="probe-tip"
          style={
            hoverX > W * 0.6
              ? { right: W - hoverX + 12 }
              : { left: hoverX + 12 }
          }
        >
          <time>
            {shown
              ? `${md(shown.start)} ${shown.count === 1 ? hm(shown.t) : `${hm(shown.start)}–${hm(shown.end)}`}`
              : `${md(hover.t)} ${hm(hover.t)}`}
          </time>
          {shown && shown.ms !== null && (
            <div>
              <i className="probe-key-line" />
              <b>{shown.ms} ms</b>
              {shown.max !== null && shown.max > shown.ms
                ? `最高 ${shown.max} ms`
                : "连接延迟"}
            </div>
          )}
          {shown && shown.fail > 0 && (
            <div>
              <i className="probe-key-fail" />
              <b>{shown.count === 1 ? "连接失败" : `失败 ${shown.fail} 次`}</b>
            </div>
          )}
          {!shown && (
            <div>
              <i className="probe-key-none" />
              <b>无数据</b>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ProbeTable({
  samples,
  hours,
  now,
}: {
  samples: ProbeSample[];
  hours: number;
  now: number;
}) {
  const rows = tableRows(samples, now, hours, zoneOf(now));
  return (
    <div className="table-wrap probe-table">
      <table>
        <thead>
          <tr>
            {["时间段", "探测", "失败", "平均延迟", "最高延迟"].map((h) => (
              <th key={h}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.start}>
              <td>
                {hm(row.start)}–{hm(row.end)}
                {/* Only a period on another day carries its date. */}
                {md(row.start) !== md(now) && <small>{md(row.start)}</small>}
              </td>
              <td>{row.ok + row.fail}</td>
              <td>{row.fail}</td>
              <td>{row.avg === null ? "无数据" : ms(row.avg)}</td>
              <td>{ms(row.max)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ProbeCard({
  route,
  hours,
  now,
  interval,
  table,
}: {
  route: ProbeRoute;
  hours: number;
  now: number;
  interval: number;
  table: boolean;
}) {
  const step = probeStep(route.samples, interval);
  const visible = visibleSamples(route.samples, now, hours);
  const stats = probeStats(visible);
  const status = probeStatus(route.samples, now, step);
  const since = status === "fail" ? failingSince(route.samples) : null;
  const last = route.samples[route.samples.length - 1];
  // Why a route has no chart; routes with a history keep showing it.
  const idle =
    route.state === "paused"
      ? "线路已暂停，暂停期间不探测，恢复后自动继续。"
      : route.state === "unsupported"
        ? "这条线路暂不支持探针。"
        : route.samples.length
          ? ""
          : route.state === "active"
            ? "正在等待第一次探测结果。"
            : "线路就绪后自动开始探测。";
  return (
    <div className="card probe-card">
      <div className="probe-head">
        <h3>
          {route.routeName}
          <Badge>L{Math.max(1, Number(route.level) || 1)}</Badge>
        </h3>
        <div className="probe-state">
          {route.state === "paused" ? (
            <Badge>已暂停</Badge>
          ) : route.state === "unsupported" ? (
            <Badge>暂不支持</Badge>
          ) : route.state !== "active" ? (
            <Badge>等待线路就绪</Badge>
          ) : status === "ok" ? (
            <Badge tone="green">连接正常</Badge>
          ) : status === "fail" ? (
            <>
              {since !== null && <small>{hm(since)} 起</small>}
              <Badge tone="red">连接失败</Badge>
            </>
          ) : (
            <Badge>暂无数据</Badge>
          )}
        </div>
      </div>
      {idle ? (
        <p className="probe-idle">{idle}</p>
      ) : (
        <div className={`probe-body ${table ? "probe-body-table" : ""}`}>
          <div>
            <div className="probe-now">
              <small>当前连接延迟</small>
              <div className="stat-value">
                {route.state === "active" && status === "ok" ? (
                  <>
                    {last[1]}
                    <span>ms</span>
                  </>
                ) : (
                  "—"
                )}
              </div>
            </div>
            <div className="probe-facts">
              {[
                ["平均延迟", ms(stats.avg)],
                ["最高延迟", ms(stats.max)],
                ["连接成功率", rateText(stats.rate)],
                ["失败次数", `${stats.fail} 次`],
              ].map(([label, text]) => (
                <div key={label}>
                  <small>{label}</small>
                  <b>{text}</b>
                </div>
              ))}
            </div>
          </div>
          {table ? (
            <ProbeTable samples={visible} hours={hours} now={now} />
          ) : (
            <ProbeChart
              samples={visible}
              hours={hours}
              now={now}
              step={step}
              label={`${route.routeName}近 ${hours} 小时：平均连接延迟${stats.avg === null ? "无数据" : ` ${stats.avg} 毫秒`}，连接成功率 ${rateText(stats.rate)}，失败 ${stats.fail} 次。按左右方向键逐段查看。`}
            />
          )}
        </div>
      )}
    </div>
  );
}

const ranges = [1, 6, 24];

/** 捐赠权益 → 探针. */
export function ProbePage({
  onNavigate,
}: {
  onNavigate: (view: string) => void;
}) {
  const { data, error, loading, reload } = useData("/api/user/probe");
  const [hours, setHours] = useState(24),
    [table, setTable] = useState(false);
  useEffect(() => {
    // Hidden tabs stop asking and refresh once when they are shown again.
    const refresh = () => {
      if (!document.hidden) reload();
    };
    const timer = window.setInterval(refresh, 60000);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  const routes: ProbeRoute[] = Array.isArray(data?.routes) ? data.routes : [];
  const interval = Math.max(60, Number(data?.intervalSeconds) || 120) * 1000;
  const now = Number(data?.now) || Date.now();
  const minutes = Math.round(interval / 60000);
  const active = routes.filter((route) => route.state === "active");
  let ok = 0,
    okCount = 0,
    failCount = 0,
    latest = 0;
  const failing: number[] = [];
  for (const route of active) {
    const status = probeStatus(
      route.samples,
      now,
      probeStep(route.samples, interval),
    );
    if (status === "ok") ok++;
    if (status === "fail") failing.push(failingSince(route.samples) ?? now);
  }
  // The totals cover every route with a history, like the cards below, also
  // while a route is waiting and not being probed.
  for (const route of routes) {
    const stats = probeStats(visibleSamples(route.samples, now, hours));
    okCount += stats.ok;
    failCount += stats.fail;
    const last = route.samples[route.samples.length - 1];
    if (last) latest = Math.max(latest, last[0]);
  }
  const allDown = active.length > 0 && failing.length === active.length;
  return (
    <div className="probe-root">
      <Header
        title="探针"
        sub={`每 ${minutes} 分钟从各线路出口连接一次你的 MSBOOST 节点，记录 TCP 连接延迟。`}
      >
        <Button onClick={reload}>刷新</Button>
      </Header>
      <ErrorNotice error={error} />
      {!data ? (
        loading ? (
          <Loading />
        ) : null
      ) : data.enabled === false ? (
        <div className="card">
          <Empty>探针暂时关闭。</Empty>
        </div>
      ) : !routes.length ? (
        <div className="card">
          <div className="empty">
            <Info size={26} />
            <p>配置中转线路后，这里会自动开始记录。</p>
            <Button primary onClick={() => onNavigate("routes")}>
              去配置中转线路
            </Button>
          </div>
        </div>
      ) : (
        <>
          {allDown && (
            <div className="mb16">
              <Notice tone="orange">
                所有线路从 {hm(Math.min(...failing))} 起都连不上你的 MSBOOST
                节点，通常是节点服务或 VPS 出了问题。请先确认 VPS 在线、MSBOOST
                服务正在运行。
              </Notice>
            </div>
          )}
          <div className="grid4">
            {[
              ["监控线路", active.length, "条"],
              ["当前连接正常", ok, "条"],
              [
                `近 ${hours} 小时连接成功率`,
                rateText(
                  okCount + failCount ? okCount / (okCount + failCount) : null,
                ),
                "",
              ],
              ["最近探测", latest ? hm(latest) : "—", ""],
            ].map(([label, value, unit]) => (
              <div className="card stat" key={label}>
                <div className="stat-top">{label}</div>
                <div className="stat-value">
                  {value}
                  {unit && <span>{unit}</span>}
                </div>
              </div>
            ))}
          </div>
          <div className="probe-bar">
            <div className="probe-controls">
              <div className="probe-seg" role="group" aria-label="时间范围">
                {ranges.map((range) => (
                  <button
                    key={range}
                    type="button"
                    className={hours === range ? "active" : ""}
                    aria-pressed={hours === range}
                    onClick={() => setHours(range)}
                  >
                    {range} 小时
                  </button>
                ))}
              </div>
              <div className="probe-seg" role="group" aria-label="显示方式">
                <button
                  type="button"
                  className={table ? "" : "active"}
                  aria-pressed={!table}
                  onClick={() => setTable(false)}
                >
                  <ChartLine size={15} />
                  图表
                </button>
                <button
                  type="button"
                  className={table ? "active" : ""}
                  aria-pressed={table}
                  onClick={() => setTable(true)}
                >
                  <Table2 size={15} />
                  表格
                </button>
              </div>
            </div>
            {!table && (
              <div className="probe-legend" aria-label="图例">
                <span>
                  <i className="probe-key-line" />
                  连接延迟
                </span>
                <span>
                  <i className="probe-key-ok" />
                  连接成功
                </span>
                <span>
                  <i className="probe-key-fail" />
                  连接失败
                </span>
                <span>
                  <i className="probe-key-none" />
                  无数据
                </span>
              </div>
            )}
          </div>
          <div className="probe-list">
            {routes.map((route) => (
              <ProbeCard
                key={route.routeId}
                route={route}
                hours={hours}
                now={now}
                interval={interval}
                table={table}
              />
            ))}
          </div>
          <div className="mt16">
            <Notice>
              探针只测量线路出口到你的 MSBOOST
              节点这一段，不包含你的电脑到线路入口和中间节点，也不等于游戏内延迟。「无数据」表示这段时间没有探测结果，不代表中转中断。
            </Notice>
          </div>
        </>
      )}
    </div>
  );
}

/** Settings → 探针. */
export function RelayProbeSettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, loading } = useData("/api/admin/settings");
  const [values, setValues] = useState<RecordData | null>(null),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  useEffect(() => {
    if (data) setValues({ ...(data.settings || data).relayProbe });
  }, [data]);
  if (loading && !values) return <Notice>正在读取探针设置…</Notice>;
  if (!values) return <ErrorNotice error={error || "探针设置读取失败"} />;
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setFailure("");
        setMessage("");
        try {
          await post("/api/admin/settings", { relayProbe: values }, "PUT");
          setMessage("探针设置已保存");
          onSaved();
        } catch (err) {
          setFailure((err as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Notice>
        开启后，网站定时让每条捐赠权益线路的出口节点连接一次用户自己的 MSBOOST
        节点，用户在「探针」页查看最近 24
        小时的连接延迟和成功率。探测不经过中转进程，不计流量；每次探测会让出口节点多同步一次，线路多时可以把间隔调大。同一出口节点每
        5 秒最多探测一条线路，线路太多时实际间隔会自动变长。
      </Notice>
      <Check
        label="开启探针"
        checked={!!values.enabled}
        onChange={(e) => setValues({ ...values, enabled: e.target.checked })}
      />
      <div className="form-grid mt16">
        <Field
          label="探测间隔（分钟）"
          type="number"
          min={1}
          max={30}
          value={values.intervalMinutes}
          hint="1–30 分钟，对所有线路生效"
          onChange={(e) => {
            const n = Math.trunc(Number(e.target.value));
            setValues({
              ...values,
              intervalMinutes: Number.isFinite(n)
                ? Math.min(30, Math.max(1, n))
                : 2,
            });
          }}
        />
      </div>
      {failure && <Notice tone="red">{failure}</Notice>}
      {message && <Notice tone="green">{message}</Notice>}
      <div className="form-actions">
        <Button type="submit" primary disabled={busy}>
          {busy ? "正在保存…" : "保存"}
        </Button>
      </div>
    </form>
  );
}
