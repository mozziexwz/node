// Calculations behind the probe page (探针), kept free of React so they can
// be tested directly. A sample is one answered probe: [time in ms, connection
// time in ms], with -1 for a connection that failed.
export type ProbeSample = [number, number];
export type ProbeRoute = {
  routeId: string;
  routeName: string;
  level: number;
  state: string;
  samples: ProbeSample[];
};
export type ProbePoint = {
  start: number;
  end: number;
  count: number;
  t: number;
  ms: number | null;
  max: number | null;
  fail: number;
};
export type ProbeSegment = {
  status: "ok" | "fail" | "none";
  a: number;
  b: number;
};

const minute = 60000;

export function visibleSamples(
  samples: ProbeSample[],
  now: number,
  hours: number,
) {
  const from = now - hours * 3600000;
  return samples.filter(([at]) => at > from);
}

export function probeStats(samples: ProbeSample[]) {
  let ok = 0,
    fail = 0,
    sum = 0,
    max = 0;
  for (const [, ms] of samples) {
    if (ms < 0) {
      fail++;
      continue;
    }
    ok++;
    sum += ms;
    max = Math.max(max, ms);
  }
  return {
    ok,
    fail,
    avg: ok ? Math.round(sum / ok) : null,
    max: ok ? max : null,
    rate: ok + fail ? ok / (ok + fail) : null,
  };
}

// Rounded down, so a single failure never shows as 100%.
export function rateText(rate: number | null) {
  if (rate === null) return "—";
  if (rate === 1) return "100%";
  return (Math.floor(rate * 1000) / 10).toFixed(1) + "%";
}

// When the newest probes all failed: the time of the first of them.
export function failingSince(samples: ProbeSample[]) {
  let i = samples.length - 1;
  if (i < 0 || samples[i][1] >= 0) return null;
  while (i > 0 && samples[i - 1][1] < 0) i--;
  return samples[i][0];
}

// The time one probe stands for. It is the configured interval, or the usual
// distance between results when a busy node is probed less often than that.
export function probeStep(samples: ProbeSample[], intervalMs: number) {
  const gaps: number[] = [];
  for (let i = 1; i < samples.length; i++)
    gaps.push(samples[i][0] - samples[i - 1][0]);
  gaps.sort((a, b) => a - b);
  const usual = gaps.length ? gaps[gaps.length >> 1] : 0;
  return Math.max(intervalMs, Math.min(usual, 30 * minute));
}

// A route's state right now. Results older than a few probes say nothing.
export function probeStatus(
  samples: ProbeSample[],
  now: number,
  step: number,
): "ok" | "fail" | "none" {
  const last = samples[samples.length - 1];
  if (!last || now - last[0] > 3 * step + minute) return "none";
  return last[1] >= 0 ? "ok" : "fail";
}

// The result bar: what each stretch of time is known to have been. Time no
// probe covers is "none", which is not the same as a failure.
export function statusSegments(
  samples: ProbeSample[],
  step: number,
  from: number,
  to: number,
) {
  const out: ProbeSegment[] = [];
  const push = (status: ProbeSegment["status"], a: number, b: number) => {
    a = Math.max(a, from);
    b = Math.min(b, to);
    if (b <= a) return;
    const last = out[out.length - 1];
    if (last && last.status === status && a <= last.b) last.b = b;
    else out.push({ status, a, b });
  };
  const near = 2.5 * step;
  let cursor = from;
  samples.forEach(([at, ms], i) => {
    const prev = samples[i - 1],
      next = samples[i + 1];
    const a = prev
      ? at - prev[0] <= near
        ? (prev[0] + at) / 2
        : at - step / 2
      : at - from <= near
        ? from
        : at - step / 2;
    const b = next
      ? next[0] - at <= near
        ? (at + next[0]) / 2
        : at + step / 2
      : to - at <= near
        ? to
        : at + step / 2;
    if (a > cursor) push("none", cursor, a);
    push(ms >= 0 ? "ok" : "fail", Math.max(a, cursor), b);
    cursor = Math.max(cursor, b);
  });
  if (cursor < to) push("none", cursor, to);
  return out;
}

function median(sorted: number[]) {
  const half = sorted.length >> 1;
  return sorted.length % 2
    ? sorted[half]
    : Math.round((sorted[half - 1] + sorted[half]) / 2);
}

function point(samples: ProbeSample[], start: number, end: number): ProbePoint {
  const ok = samples
    .filter(([, ms]) => ms >= 0)
    .map(([, ms]) => ms)
    .sort((a, b) => a - b);
  return {
    start,
    end,
    count: samples.length,
    t: samples.reduce((sum, [at]) => sum + at, 0) / samples.length,
    ms: ok.length ? median(ok) : null,
    max: ok.length ? ok[ok.length - 1] : null,
    fail: samples.length - ok.length,
  };
}

export function bucketMinutes(hours: number) {
  return hours >= 24 ? 10 : hours >= 6 ? 6 : 0;
}

// Points of the line. Longer ranges take the median of each clock-aligned
// bucket, so one slow probe does not draw a spike; every probe still shows in
// the result bar. zone is the local offset from UTC in ms (UTC minus local).
export function linePoints(
  samples: ProbeSample[],
  hours: number,
  zone: number,
) {
  const size = bucketMinutes(hours) * minute;
  if (!size)
    return samples.map((sample) => point([sample], sample[0], sample[0]));
  const out: ProbePoint[] = [];
  let start = NaN,
    group: ProbeSample[] = [];
  const flush = () => {
    if (group.length) out.push(point(group, start, start + size));
  };
  for (const sample of samples) {
    const bucket = Math.floor((sample[0] - zone) / size) * size + zone;
    if (bucket !== start) {
      flush();
      start = bucket;
      group = [];
    }
    group.push(sample);
  }
  flush();
  return out;
}

// The line is broken where no probe succeeded or none was made.
export function lineRuns(points: ProbePoint[], breakGap: number) {
  const runs: ProbePoint[][] = [];
  let run: ProbePoint[] | null = null,
    previous: ProbePoint | null = null;
  for (const p of points) {
    if (p.ms === null) run = null;
    else {
      if (!run || !previous || p.t - previous.t > breakGap)
        runs.push((run = []));
      run.push(p);
    }
    previous = p.ms === null ? null : p;
  }
  return runs;
}

// The table view: every period of the range, newest first.
export function tableRows(
  samples: ProbeSample[],
  now: number,
  hours: number,
  zone: number,
) {
  const size = (hours >= 24 ? 240 : hours >= 6 ? 60 : 10) * minute;
  const from = now - hours * 3600000;
  const rows = [];
  for (
    let start = Math.floor((from - zone) / size) * size + zone;
    start <= now;
    start += size
  ) {
    const inside = samples.filter(
      ([at]) => at >= start && at < start + size && at > from,
    );
    rows.push({ start, end: start + size, ...probeStats(inside) });
  }
  return rows.reverse();
}

// The scale ends on a round number and uses two or three intervals.
export function niceScale(max: number) {
  const steps = [5, 10, 20, 25, 50, 100, 200, 250, 500, 1000, 2000];
  const target = max * 1.08;
  for (const step of steps) {
    const n = Math.ceil(target / step);
    if (n <= 3) return { step, top: Math.max(2, n) * step };
  }
  return { step: 2000, top: Math.ceil(target / 2000) * 2000 };
}

// Clock-aligned labels of the time axis.
export function xTicks(from: number, to: number, hours: number) {
  const every = hours >= 24 ? 240 : hours >= 6 ? 60 : 10;
  const d = new Date(from);
  const minutes = d.getHours() * 60 + d.getMinutes() + 1;
  d.setHours(0, Math.ceil(minutes / every) * every, 0, 0);
  const out: number[] = [];
  for (let t = d.getTime(); t <= to; t += every * minute) out.push(t);
  return out;
}
