import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";

// The homepage's sections are numbered and alternate between white and a warm
// band; a table of contents follows the reader and a jump marks where it
// landed. Every request is answered by fixtures.
const base = "http://127.0.0.1:19992";
const item = (title) => ({ title, desc: title + "说明" });
const download = {
  version: "v1.1.0",
  releasedAt: "2026-10-24",
  downloadUrl: "https://file.msboost.de/#/?code=Launcher",
  backupUrl: "",
  sha256: "",
  notes: "",
};
const home = {
  titleLine1: "一键部署你的MapleStory",
  titleLine2: "独立IP游戏节点",
  showExplain: true,
  showLauncher: true,
  features: [
    { title: "一键部署 MSBOOST", desc: "部署节点", target: "#msboost" },
    { title: "一键配置中转服务器", desc: "共享线路", target: "#relay" },
    { title: "MSBOOST 启动器", desc: "一键加速", target: "#launcher" },
  ],
  msboost: {
    eyebrow: "MSBOOST 节点",
    title: "什么是 MSBOOST？",
    lead: "节点说明",
    nodes: [item("你的电脑"), item("你的节点"), item("游戏服务器")],
    points: [item("要点一")],
  },
  relay: {
    eyebrow: "中转服务器",
    title: "什么是中转服务器？",
    lead: "中转说明",
    direct: [item("电脑"), item("节点"), item("游戏")],
    via: [item("电脑"), item("中转"), item("节点"), item("游戏")],
    points: [item("要点二")],
  },
};
const articles = [
  {
    id: "a1",
    title: "第一篇",
    category: "资讯",
    summary: "摘要",
    createdAt: 1761300000000,
  },
];

let script, css;
async function homepage(run, { reducedMotion = "no-preference" } = {}) {
  if (!script) {
    const compiled = await build({
      entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
      bundle: true,
      write: false,
      outdir: "in-memory",
      format: "iife",
      platform: "browser",
    });
    script = compiled.outputFiles.find((f) => f.path.endsWith(".js")).text;
    css = compiled.outputFiles.find((f) => f.path.endsWith(".css")).text;
  }
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({
    viewport: { width: 1440, height: 900 },
    reducedMotion,
  });
  page.setDefaultTimeout(7000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      errors.push("external request " + url.origin);
      return route.abort();
    }
    if (url.pathname === "/home")
      return route.fulfill({
        contentType: "text/html",
        body: `<!doctype html><meta charset="utf-8"><style>${css}</style><div id="root"></div><script>${script}</script>`,
      });
    if (url.pathname === "/api/public/home")
      return route.fulfill({
        json: {
          home,
          site: {},
          articles,
          launcher: download,
          localApp: { ...download, version: "v1.0.0" },
        },
      });
    return route.fulfill({ json: {} });
  });
  try {
    await page.goto(base + "/home");
    await page.locator(".hp .public-hero").waitFor();
    await run(page);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

test(
  "sections are numbered, banded and given subheadings",
  { timeout: 60000 },
  async () => {
    await homepage(async (page) => {
      await expect(page.locator(".home-explain .hp-num")).toHaveText([
        "01",
        "02",
        "03",
        "04",
      ]);
      await expect(page.locator(".public-tools .hp-card-num")).toHaveText([
        "01",
        "02",
        "03",
      ]);
      const bands = await page
        .locator(".home-explain, .public-latest")
        .evaluateAll((sections) =>
          sections.map((s) => [s.id, s.classList.contains("hp-band")]),
        );
      assert.deepEqual(bands, [
        ["msboost", true],
        ["relay", false],
        ["launcher", true],
        ["local", false],
        ["latest", true],
      ]);
      await expect(
        page.locator("#launcher .hp-sub, #local .hp-sub"),
      ).toHaveText(["三步开始使用", "注意事项", "三步开始使用", "注意事项"]);
      await expect(
        page.locator("#launcher").getByRole("link", { name: "使用指南" }),
      ).toHaveAttribute("href", "/guides/launcher.html");
      await expect(
        page.locator("#local").getByRole("link", { name: "使用指南" }),
      ).toHaveAttribute("href", "/guides/local.html");
    });
  },
);

test(
  "the table of contents follows the reader and a jump marks where it landed",
  { timeout: 60000 },
  async () => {
    await homepage(async (page) => {
      const bar = page.locator(".hp-secnav");
      await expect(bar).not.toHaveClass(/show/);
      await expect(bar.getByRole("link")).toHaveText([
        "01什么是 MSBOOST",
        "02中转服务器",
        "03启动器",
        "04本地版",
        "公告与教程",
      ]);
      await page.locator('.public-tools a[href="#relay"]').click();
      await expect(page.locator("#relay")).toHaveAttribute("data-landed", "");
      await expect(bar).toHaveClass(/show/);
      await expect(
        bar.getByRole("link", { name: "02 中转服务器" }),
      ).toHaveAttribute("aria-current", "true");
      await expect(page).toHaveURL(/\/home#relay$/);
      // The section starts right under the header and the bar.
      const gap = await page.evaluate(() => {
        const head = document
          .querySelector(".public-header")
          .getBoundingClientRect().bottom;
        return (
          document.getElementById("relay").getBoundingClientRect().top -
          head -
          document.querySelector(".hp-secnav").offsetHeight
        );
      });
      assert.ok(Math.abs(gap) <= 2, `section lands ${gap}px below the bar`);
      await expect(
        page.locator("#relay [data-reveal][data-in]").first(),
      ).toBeVisible();
      await bar.getByRole("link", { name: "04 本地版" }).click();
      await expect(
        bar.getByRole("link", { name: "04 本地版" }),
      ).toHaveAttribute("aria-current", "true");
      const top = page.getByRole("button", { name: "回到顶部" });
      await expect(top).toHaveClass(/show/);
      await top.click();
      await expect.poll(() => page.evaluate(() => scrollY)).toBe(0);
      await expect(bar).not.toHaveClass(/show/);
    });
  },
);

test(
  "reduced motion shows everything at once",
  { timeout: 60000 },
  async () => {
    await homepage(
      async (page) => {
        await expect(page.locator("[data-reveal]")).toHaveCount(0);
        await page.locator('.public-tools a[href="#launcher"]').click();
        await expect(page.locator("#launcher")).toBeInViewport();
      },
      { reducedMotion: "reduce" },
    );
  },
);
