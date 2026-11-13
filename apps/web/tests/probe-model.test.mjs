import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import ts from "typescript";

const output = ts.transpileModule(
  fs.readFileSync(new URL("../src/probe-model.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText;
const model = await import(
  "data:text/javascript;base64," + Buffer.from(output).toString("base64")
);

const minute = 60000;
const step = 2 * minute;
// One minute past the hour, so the newest probe opens a period of its own.
const now = Date.UTC(2026, 10, 12, 12, 1, 0);
// One probe every two minutes, oldest first: [minutes ago, connection time].
const every = (from, to, ms) => {
  const out = [];
  for (let ago = from; ago >= to; ago -= 2) out.push([now - ago * minute, ms]);
  return out;
};

test("statistics count failures apart and never round a failure away", () => {
  const stats = model.probeStats([
    [1, 100],
    [2, 140],
    [3, -1],
  ]);
  assert.deepEqual(stats, { ok: 2, fail: 1, avg: 120, max: 140, rate: 2 / 3 });
  assert.equal(model.rateText(1), "100%");
  assert.equal(model.rateText(0.99995), "99.9%");
  assert.equal(model.rateText(null), "—");
  assert.deepEqual(model.probeStats([]), {
    ok: 0,
    fail: 0,
    avg: null,
    max: null,
    rate: null,
  });
});

test("the current state needs a recent result", () => {
  assert.equal(model.probeStatus(every(10, 0, 30), now, step), "ok");
  assert.equal(
    model.probeStatus([...every(10, 4, 30), ...every(2, 0, -1)], now, step),
    "fail",
  );
  // The newest result is twenty minutes old: nothing is known about now.
  assert.equal(model.probeStatus(every(40, 20, 30), now, step), "none");
  assert.equal(model.probeStatus([], now, step), "none");
  const samples = [...every(20, 8, 30), ...every(6, 0, -1)];
  assert.equal(model.failingSince(samples), now - 6 * minute);
  assert.equal(model.failingSince(every(6, 0, 30)), null);
});

test("the result bar separates failures from time without data", () => {
  const samples = [
    ...every(58, 40, 30),
    ...every(38, 34, -1),
    // nothing between 32 and 12 minutes ago
    ...every(10, 0, 30),
  ];
  const from = now - 60 * minute;
  const segments = model.statusSegments(samples, step, from, now);
  assert.deepEqual(
    segments.map((s) => s.status),
    ["ok", "fail", "none", "ok"],
  );
  // The bar has no holes and no overlaps, and spans the whole range.
  assert.equal(segments[0].a, from);
  assert.equal(segments.at(-1).b, now);
  for (let i = 1; i < segments.length; i++)
    assert.equal(segments[i].a, segments[i - 1].b);
  // The failure covers its three probes, one step each.
  assert.equal(segments[1].b - segments[1].a, 3 * step);
  assert.deepEqual(model.statusSegments([], step, from, now), [
    { status: "none", a: from, b: now },
  ]);
});

test("a slower schedule is not shown as missing data", () => {
  // A busy node answers every six minutes although two were asked for.
  const samples = [];
  for (let ago = 60; ago >= 0; ago -= 6) samples.push([now - ago * minute, 30]);
  const usual = model.probeStep(samples, step);
  assert.equal(usual, 6 * minute);
  assert.deepEqual(
    model
      .statusSegments(samples, usual, now - 60 * minute, now)
      .map((s) => s.status),
    ["ok"],
  );
  // A single long outage does not stretch the step.
  assert.equal(
    model.probeStep([...every(600, 500, 30), ...every(20, 0, 30)], step),
    step,
  );
});

test("long ranges draw the median of each period and break at gaps", () => {
  const samples = every(58, 0, 100);
  samples[3][1] = 900; // one slow probe
  samples[10][1] = -1; // one failed probe
  const six = model.linePoints(samples, 6, 0);
  assert.ok(six.length <= 11 && six.length >= 10);
  assert.ok(six.every((p) => p.ms === 100));
  assert.equal(Math.max(...six.map((p) => p.max)), 900);
  assert.equal(
    six.reduce((sum, p) => sum + p.fail, 0),
    1,
  );
  // One hour shows every probe as it was.
  const one = model.linePoints(samples, 1, 0);
  assert.equal(one.length, samples.length);
  assert.equal(one[3].ms, 900);
  assert.equal(one[10].ms, null);
  assert.equal(model.lineRuns(one, 2.5 * step).length, 2);
  // A stretch without probes also breaks the line.
  const apart = model.linePoints(
    [...every(58, 40, 100), ...every(10, 0, 100)],
    1,
    0,
  );
  assert.equal(model.lineRuns(apart, 2.5 * step).length, 2);
});

test("the table lists every period, newest first", () => {
  const samples = [...every(58, 40, 100), ...every(10, 0, -1)];
  const rows = model.tableRows(samples, now, 1, 0);
  // 11:00 to 12:10 in ten-minute periods; the newest holds the latest probe.
  assert.equal(rows.length, 7);
  assert.equal(rows[0].fail, 1);
  assert.equal(rows[1].fail, 5);
  assert.equal(rows[1].avg, null);
  // 11:30–11:50 had no probes at all.
  assert.deepEqual(
    rows.slice(2, 4).map((r) => r.ok + r.fail),
    [0, 0],
  );
  assert.equal(rows[6].avg, 100);
  assert.equal(
    rows.reduce((sum, r) => sum + r.ok + r.fail, 0),
    samples.length,
  );
});

test("the scale ends on a round number", () => {
  assert.deepEqual(model.niceScale(108), { step: 50, top: 150 });
  assert.deepEqual(model.niceScale(215), { step: 100, top: 300 });
  assert.deepEqual(model.niceScale(19), { step: 10, top: 30 });
  assert.deepEqual(model.niceScale(1), { step: 5, top: 10 });
});
