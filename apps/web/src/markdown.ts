import { Marked, type TokenizerAndRendererExtension } from "marked";

// Articles are written by people who expect what they type to be what they
// see: Enter starts a new line, extra blank lines leave room, and spaces keep
// their width. Standard Markdown would join lines, collapse blank lines, drop
// leading spaces and turn a 4-space indent into a code block, so the source is
// adjusted before parsing. Fenced code, inline code, tables and list nesting
// keep their Markdown meaning.

const NBSP = String.fromCharCode(0xa0);
const fence = /^ {0,3}(`{3,}|~{3,})/;
const listItem = /^(\s*(?:[-*+]|\d{1,9}[.)])[ \t]+(?:\[[ xX]\][ \t]+)?)/;
const quote = /^(\s*(?:>[ \t]?)+)/;
const heading = /^( {0,3}#{1,6}[ \t]+)/;
const tableRow = /^\s*\|/;

// Runs of two or more spaces outside inline code keep their width; the last
// space of a run stays breakable.
function keepInnerSpaces(text: string) {
  return text
    .split(/(`+[^`]*?`+)/)
    .map((part, i) =>
      i % 2
        ? part
        : part.replace(/ {2,}/g, (run) => NBSP.repeat(run.length - 1) + " "),
    )
    .join("")
    .replace(/[ \t]+$/, "");
}

function transformLine(line: string) {
  if (tableRow.test(line)) return line;
  for (const prefix of [listItem, quote, heading]) {
    const match = line.match(prefix);
    if (match) return match[1] + keepInnerSpaces(line.slice(match[1].length));
  }
  const lead = line.match(/^[ \t]+/)?.[0] || "";
  const indent = lead.replace(/\t/g, "  ").replace(/ /g, NBSP);
  return indent + keepInnerSpaces(line.slice(lead.length));
}

export function preserveLayout(source: string) {
  const out: string[] = [];
  let open = "",
    blanks = 0;
  for (const line of source.replace(/\r\n?/g, "\n").split("\n")) {
    const marker = line.match(fence)?.[1];
    if (open) {
      out.push(line);
      if (marker && marker[0] === open[0] && marker.length >= open.length)
        open = "";
      continue;
    }
    if (!line.trim()) {
      blanks++;
      continue;
    }
    if (blanks) {
      out.push("");
      // The first blank line separates paragraphs; each further one keeps a
      // line of height.
      if (blanks > 1 && out.length > 1)
        out.push(
          Array(blanks - 1)
            .fill('<p class="md-gap"></p>')
            .join("\n"),
          "",
        );
      blanks = 0;
    }
    if (marker) {
      open = marker;
      out.push(line);
      continue;
    }
    out.push(transformLine(line));
  }
  return out.join("\n");
}

// ==text== marks a highlight.
const highlight: TokenizerAndRendererExtension = {
  name: "highlight",
  level: "inline",
  start(source) {
    const index = source.indexOf("==");
    return index < 0 ? undefined : index;
  },
  tokenizer(source) {
    const match = /^==(?=\S)([\s\S]*?\S)==/.exec(source);
    if (match)
      return {
        type: "highlight",
        raw: match[0],
        tokens: this.lexer.inlineTokens(match[1]),
      };
  },
  renderer(token) {
    return `<mark>${this.parser.parseInline(token.tokens || [])}</mark>`;
  },
};

// CommonMark does not close **bold** after full-width punctuation that is
// followed by a letter, as in "**注意：**内容", which is how Chinese is
// usually written, so plain **text** is matched by its markers alone.
// Anything starting or ending with * is left to the standard rules.
const strong: TokenizerAndRendererExtension = {
  name: "strongText",
  level: "inline",
  start(source) {
    const index = source.indexOf("**");
    return index < 0 ? undefined : index;
  },
  tokenizer(source) {
    const match = /^\*\*(?=[^\s*])([\s\S]*?[^\s*])\*\*(?!\*)/.exec(source);
    if (match)
      return {
        type: "strongText",
        raw: match[0],
        tokens: this.lexer.inlineTokens(match[1]),
      };
  },
  renderer(token) {
    return `<strong>${this.parser.parseInline(token.tokens || [])}</strong>`;
  },
};

const parser = new Marked({ gfm: true, breaks: true });
parser.use({
  extensions: [highlight, strong],
  tokenizer: {
    // Indentation is layout here, never a code block; code uses ``` fences.
    code() {
      return undefined;
    },
  },
  renderer: {
    // Sanitising removes <input>, so task-list boxes are drawn with a span.
    checkbox({ checked }) {
      return `<span class="md-task${checked ? " done" : ""}" aria-hidden="true">${checked ? "✓" : ""}</span>`;
    },
  },
});

// Returns unsanitised HTML; callers must sanitise it before inserting it.
export function renderMarkdown(text: string) {
  const html = parser.parse(preserveLayout(text || ""), {
    async: false,
  }) as string;
  // Wide tables scroll inside their own box instead of widening the page.
  return html
    .replace(/<table>/g, '<div class="md-table"><table>')
    .replace(/<\/table>/g, "</table></div>");
}
