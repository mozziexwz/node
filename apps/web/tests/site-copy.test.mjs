import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Wording rules for everything the web app can show.
const web = fileURLToPath(new URL("..", import.meta.url));
function sources(folder) {
  return fs.readdirSync(folder, { withFileTypes: true }).flatMap((entry) => {
    const file = path.join(folder, entry.name);
    if (entry.isDirectory()) return sources(file);
    return /\.(tsx?|css|html)$/.test(entry.name) ? [file] : [];
  });
}
const files = [...sources(path.join(web, "src")), path.join(web, "index.html")];

test("the site says MapleStory and 中转, and no longer mentions the BAT launcher", () => {
  for (const file of files) {
    const text = fs.readFileSync(file, "utf8");
    for (const word of ["冒险岛", "转发", "旧版 BAT", "BAT 启动器", "v3.35.0"])
      assert.ok(
        !text.includes(word),
        `${path.relative(web, file)} contains ${word}`,
      );
  }
});

test("public pages show no source-code host, licence or 「新」 badges", () => {
  for (const name of [
    "public-site.tsx",
    "launcher.tsx",
    "auth.tsx",
    "ui.tsx",
  ]) {
    const text = fs.readFileSync(path.join(web, "src", name), "utf8");
    assert.doesNotMatch(text, /github|GPL|源代码/i, name);
    assert.doesNotMatch(text, /[>"「]新[<"」]/, name);
  }
});
