import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import ts from "typescript";

const source = fs.readFileSync(
  new URL("../src/users-model.ts", import.meta.url),
  "utf8",
);
const output = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText;
const { unitInteger, integerUnit, userDraft, userPatch, sortedUsers, DAY_MS } =
  await import(
    "data:text/javascript;base64," + Buffer.from(output).toString("base64")
  );

test("user whole-leaf/GB conversions are exact and reject unsafe input", () => {
  assert.equal(unitInteger("29", 0, "枫叶"), 29);
  assert.equal(unitInteger("12.000000001", 9, "流量"), 12000000001);
  assert.equal(integerUnit(12000000001, 9), "12.000000001");
  assert.equal(integerUnit(5000, 2), "50");
  assert.throws(() => unitInteger("1.1", 0, "枫叶"));
  assert.throws(() => unitInteger("9007199254740992", 0, "速率"));
  for (const value of ["-1", "NaN", "Infinity", "1e6", ""])
    assert.throws(() => unitInteger(value, 0, "枫叶"));
});

test("editing email never recomputes rounded entitlement deadline or traffic", () => {
  const now = 1700000000000,
    user = {
      email: "12345678@qq.com",
      role: "member",
      status: "active",
      balanceCents: 2900,
      expiresAt: now + 3 * DAY_MS + 12345,
      trafficTotal: 12000000001,
      trafficUsed: 1,
      rateMbps: 2,
    };
  const baseline = userDraft(user, now);
  const patch = userPatch(
    user,
    baseline,
    { ...baseline, email: "87654321@qq.com" },
    now + 86400000,
  );
  assert.equal(patch.email, "87654321@qq.com");
  for (const key of [
    "expiresAt",
    "balanceCents",
    "trafficTotal",
    "trafficUsed",
    "rateMbps",
  ])
    assert.equal(
      Object.hasOwn(patch, key),
      false,
      key + " changed unexpectedly",
    );
});

test("editing days explicitly sets a deadline from save time and preserves exact funds", () => {
  const now = 1700000000000,
    user = {
      email: "12345678@qq.com",
      balanceCents: 500,
      expiresAt: now + 10 * DAY_MS,
      trafficTotal: 1000000000,
      trafficUsed: 0,
      rateMbps: 1,
    };
  const baseline = userDraft(user, now),
    patch = userPatch(
      user,
      baseline,
      { ...baseline, days: "1.5", balance: "29" },
      now + 1000,
    );
  assert.equal(patch.expiresAt, now + 1000 + 1.5 * DAY_MS);
  assert.equal(patch.balanceCents, 2900);
  assert.throws(() =>
    userPatch(user, baseline, { ...baseline, total: "0", used: "1" }, now),
  );
});

test("all five user columns sort numerically without mutating source", () => {
  const now = 1700000000000,
    users = [
      {
        email: "b@qq.com",
        balanceCents: 300,
        expiresAt: now + 2 * DAY_MS,
        trafficTotal: 100,
        trafficUsed: 10,
        rateMbps: 2,
      },
      {
        email: "a@qq.com",
        balanceCents: 50,
        expiresAt: now + DAY_MS,
        trafficTotal: 10,
        trafficUsed: 2,
        rateMbps: 1,
      },
    ];
  for (const key of [
    "balanceCents",
    "days",
    "trafficTotal",
    "trafficUsed",
    "rateMbps",
  ]) {
    assert.equal(sortedUsers(users, key, "asc", now)[0].email, "a@qq.com");
    assert.equal(sortedUsers(users, key, "desc", now)[0].email, "b@qq.com");
  }
  assert.equal(users[0].email, "b@qq.com");
});
