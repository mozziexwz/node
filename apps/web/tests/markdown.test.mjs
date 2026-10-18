import { test } from "node:test";
import assert from "node:assert/strict";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// The article display rules, compiled from the real source.
const out = await build({
  entryPoints: [fileURLToPath(new URL("../src/markdown.ts", import.meta.url))],
  bundle: true,
  format: "esm",
  platform: "node",
  write: false,
  logLevel: "silent",
});
const { renderMarkdown } = await import(
  "data:text/javascript;base64," +
    Buffer.from(out.outputFiles[0].text).toString("base64")
);
const render = (text) => renderMarkdown(text).trim();
const nbsp = " ";

test("Enter starts a new line and each extra blank line keeps its height", () => {
  assert.equal(render("第一行\n第二行"), "<p>第一行<br>第二行</p>");
  const html = render("段一\n\n段二\n\n\n\n段三");
  assert.equal(html.match(/<p>段/g).length, 3);
  assert.equal(html.match(/<p class="md-gap"><\/p>/g).length, 2);
  // Windows line endings count the same.
  assert.equal(render("a\r\n\r\n\r\nb").match(/md-gap/g).length, 1);
  // Blank lines at the very start do not leave a gap.
  assert.equal(render("\n\n\n正文"), "<p>正文</p>");
});

test("spaces keep their width and indentation is never a code block", () => {
  assert.equal(render("  缩进两格"), `<p>${nbsp.repeat(2)}缩进两格</p>`);
  assert.equal(render("    缩进四格"), `<p>${nbsp.repeat(4)}缩进四格</p>`);
  assert.equal(render("\t制表符"), `<p>${nbsp.repeat(2)}制表符</p>`);
  assert.equal(
    render("中间    四个空格 `a    b`"),
    `<p>中间${nbsp.repeat(3)} 四个空格 <code>a    b</code></p>`,
  );
  // List nesting still uses indentation.
  assert.match(render("- a\n  - b"), /<li>a<ul>\s*<li>b<\/li>/);
});

test("fenced code keeps its text, blank lines included", () => {
  assert.equal(
    render("```\n  code\n\n\n  more\n```\n后面"),
    "<pre><code>  code\n\n\n  more\n</code></pre>\n<p>后面</p>",
  );
});

test("bold closes after full-width punctuation, as Chinese is written", () => {
  assert.equal(render("**注意：**内容"), "<p><strong>注意：</strong>内容</p>");
  assert.equal(
    render("注意**「重要」**事项"),
    "<p>注意<strong>「重要」</strong>事项</p>",
  );
  assert.equal(render("**a *b* c**"), "<p><strong>a <em>b</em> c</strong></p>");
  assert.equal(render("2 ** 3 ** 4"), "<p>2 ** 3 ** 4</p>");
  assert.equal(render("`a**b**c`"), "<p><code>a**b**c</code></p>");
  assert.equal(render("\\*\\*字面\\*\\*"), "<p>**字面**</p>");
});

test("highlight, task lists and tables", () => {
  assert.equal(render("==重点=="), "<p><mark>重点</mark></p>");
  assert.match(
    render("- [x] 完成\n- [ ] 未完成"),
    /<li><span class="md-task done" aria-hidden="true">✓<\/span> 完成<\/li>\s*<li><span class="md-task" aria-hidden="true"><\/span> 未完成<\/li>/,
  );
  const table = render("| 列1 | 列2 |\n| :---: | --- |\n| 内容 | 内容 |");
  assert.match(table, /^<div class="md-table"><table>/);
  assert.match(table, /<\/table><\/div>$/);
  assert.match(table, /<th align="center">列1<\/th>/);
});
