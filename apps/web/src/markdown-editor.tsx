import { useEffect, useRef } from "react";
import {
  Annotation,
  Compartment,
  EditorSelection,
  EditorState,
  Transaction,
  type ChangeSpec,
  type Line,
} from "@codemirror/state";
import {
  Decoration,
  EditorView,
  MatchDecorator,
  ViewPlugin,
  highlightActiveLine,
  highlightActiveLineGutter,
  keymap,
  lineNumbers,
  placeholder,
  type DecorationSet,
  type ViewUpdate,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
  redo,
  redoDepth,
  undo,
  undoDepth,
} from "@codemirror/commands";
import {
  HighlightStyle,
  LanguageSupport,
  syntaxHighlighting,
} from "@codemirror/language";
import { markdownKeymap, markdownLanguage } from "@codemirror/lang-markdown";
import { tags } from "@lezer/highlight";

// The article editor is only loaded when an administrator opens it, so the
// CodeMirror code stays out of the public pages.

export type MarkdownAction =
  | "bold"
  | "italic"
  | "strike"
  | "highlight"
  | "heading"
  | "bullet"
  | "ordered"
  | "quote"
  | "link"
  | "image"
  | "code"
  | "table"
  | "rule"
  | "undo"
  | "redo"
  | "clear";

export type MarkdownEditorHandle = {
  run: (action: MarkdownAction) => void;
  focus: () => void;
};

export type MarkdownEditorProps = {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  label?: string;
  // Called once the editor exists, with the commands the toolbar uses.
  onReady?: (editor: MarkdownEditorHandle) => void;
  onHistory?: (canUndo: boolean, canRedo: boolean) => void;
  // The preview panel to keep at the same relative scroll position.
  syncScroll?: HTMLElement | null;
};

// Marks text replaced from outside, which is not reported back as an edit.
const external = Annotation.define<boolean>();

function editableState(on: boolean) {
  return [EditorView.editable.of(on), EditorState.readOnly.of(!on)];
}

const markers = {
  bold: ["**", "加粗文字"],
  italic: ["*", "斜体文字"],
  strike: ["~~", "删除文字"],
  highlight: ["==", "高亮文字"],
} as const;

function selectedLines(state: EditorState) {
  const lines = new Map<number, Line>();
  for (const range of state.selection.ranges) {
    const first = state.doc.lineAt(range.from).number;
    let last = state.doc.lineAt(range.to).number;
    // A selection that ends at the start of a line does not include it.
    if (last > first && state.doc.line(last).from === range.to) last--;
    for (let n = first; n <= last; n++) lines.set(n, state.doc.line(n));
  }
  return [...lines.values()];
}

function format(
  view: EditorView,
  spec: ChangeSpec[],
  select?: EditorSelection,
) {
  const changes = view.state.changes(spec);
  view.dispatch({
    changes,
    // Text inserted at the cursor ends up before it.
    selection: select || view.state.selection.map(changes, 1),
    scrollIntoView: true,
    userEvent: "input.format",
  });
  view.focus();
}

// Wraps each selection in markers, or removes them when already wrapped.
function wrap(view: EditorView, mark: string, sample: string) {
  const { state } = view;
  const result = state.changeByRange((range) => {
    const text = state.sliceDoc(range.from, range.to),
      outer = range.from - mark.length;
    const wrapped =
      outer >= 0 &&
      state.sliceDoc(outer, range.from) === mark &&
      state.sliceDoc(range.to, range.to + mark.length) === mark &&
      // A single * next to another * belongs to bold, not italics.
      (mark.length > 1 ||
        (state.sliceDoc(outer - 1, outer) !== mark &&
          state.sliceDoc(range.to + 1, range.to + 2) !== mark));
    if (wrapped)
      return {
        changes: [
          { from: outer, to: range.from },
          { from: range.to, to: range.to + mark.length },
        ],
        range: EditorSelection.range(outer, range.to - mark.length),
      };
    if (
      text.length > mark.length * 2 &&
      text.startsWith(mark) &&
      text.endsWith(mark)
    )
      return {
        changes: {
          from: range.from,
          to: range.to,
          insert: text.slice(mark.length, -mark.length),
        },
        range: EditorSelection.range(range.from, range.to - mark.length * 2),
      };
    const inner = text || sample,
      start = range.from + mark.length;
    return {
      changes: { from: range.from, to: range.to, insert: mark + inner + mark },
      range: EditorSelection.range(start, start + inner.length),
    };
  });
  view.dispatch(
    state.update(result, { scrollIntoView: true, userEvent: "input.format" }),
  );
  view.focus();
}

// Cycles the heading of each selected line: none, ##, ###, none.
function heading(view: EditorView) {
  const spec: ChangeSpec[] = [];
  for (const line of selectedLines(view.state)) {
    const match = /^( {0,3})(#{1,6})[ \t]+/.exec(line.text);
    if (!match) spec.push({ from: line.from, insert: "## " });
    else if (match[2] === "###")
      spec.push({ from: line.from, to: line.from + match[0].length });
    else
      spec.push({
        from: line.from + match[1].length,
        to: line.from + match[1].length + match[2].length,
        insert: match[2] === "##" ? "###" : "##",
      });
  }
  format(view, spec);
}

const lineMarks = {
  bullet: /^(\s*)[-*+][ \t]+/,
  ordered: /^(\s*)\d{1,9}[.)][ \t]+/,
  quote: /^(\s*)>[ \t]?/,
};
const anyListMark = /^(\s*)(?:[-*+]|\d{1,9}[.)])[ \t]+/;

// Adds the marker to each selected line, or removes it when every line
// already has it. Another kind of list marker is replaced, not stacked.
function prefixLines(view: EditorView, kind: keyof typeof lineMarks) {
  const lines = selectedLines(view.state),
    written = lines.filter((line) => line.text.trim()),
    targets = written.length ? written : lines,
    pattern = lineMarks[kind],
    remove = targets.every((line) => pattern.test(line.text)),
    spec: ChangeSpec[] = [];
  targets.forEach((line, index) => {
    const own = pattern.exec(line.text);
    if (remove) {
      if (own)
        spec.push({
          from: line.from + own[1].length,
          to: line.from + own[0].length,
        });
      return;
    }
    if (kind === "quote") {
      spec.push({ from: line.from, insert: "> " });
      return;
    }
    const marker = kind === "bullet" ? "- " : `${index + 1}. `,
      other = anyListMark.exec(line.text),
      indent = /^\s*/.exec(line.text)![0].length;
    spec.push(
      other
        ? {
            from: line.from + other[1].length,
            to: line.from + other[0].length,
            insert: marker,
          }
        : { from: line.from + indent, insert: marker },
    );
  });
  format(view, spec);
}

function link(view: EditorView, image: boolean) {
  const { state } = view;
  const result = state.changeByRange((range) => {
    const text = state.sliceDoc(range.from, range.to).trim(),
      isUrl = /^https?:\/\/\S+$/.test(text),
      label = (!isUrl && text) || (image ? "图片说明" : "链接文字"),
      url = isUrl ? text : "https://",
      prefix = image ? "![" : "[",
      insert = `${prefix}${label}](${url})`,
      labelFrom = range.from + prefix.length,
      urlFrom = range.from + insert.length - 1 - url.length;
    // Select what is still to be typed: the address, unless an address was
    // selected or an empty link still needs its text.
    const pickUrl = !isUrl && (image || text !== "");
    return {
      changes: { from: range.from, to: range.to, insert },
      range: pickUrl
        ? EditorSelection.range(urlFrom, urlFrom + url.length)
        : EditorSelection.range(labelFrom, labelFrom + label.length),
    };
  });
  view.dispatch(
    state.update(result, { scrollIntoView: true, userEvent: "input.format" }),
  );
  view.focus();
}

// Inserts a block on its own lines, one blank line away from any text, and
// selects [selectFrom, selectTo) inside it.
function insertBlock(
  view: EditorView,
  block: string,
  selectFrom = block.length,
  selectTo = selectFrom,
) {
  const { state } = view,
    line = state.doc.lineAt(state.selection.main.head),
    blank = !line.text.trim(),
    above = blank
      ? line.number > 1 && !!state.doc.line(line.number - 1).text.trim()
      : true,
    below =
      line.number < state.doc.lines &&
      !!state.doc.line(line.number + 1).text.trim(),
    lead = above ? (blank ? "\n" : "\n\n") : "",
    from = blank ? line.from : line.to,
    start = from + lead.length;
  format(
    view,
    [{ from, to: line.to, insert: lead + block + (below ? "\n" : "") }],
    EditorSelection.single(start + selectFrom, start + selectTo),
  );
}

// Single-line selections become inline code; several lines, or the cursor
// on an empty line, become a code block.
function code(view: EditorView) {
  const { state } = view,
    range = state.selection.main,
    text = state.sliceDoc(range.from, range.to);
  if (text.includes("\n")) {
    const from = state.doc.lineAt(range.from).from,
      to = state.doc.lineAt(range.to).to,
      body = state.sliceDoc(from, to);
    format(
      view,
      [{ from, to, insert: "```\n" + body + "\n```" }],
      EditorSelection.single(from + 4, from + 4 + body.length),
    );
  } else if (!text && !state.doc.lineAt(range.head).text.trim())
    insertBlock(view, "```\n\n```", 4);
  else wrap(view, "`", "代码");
}

function clear(view: EditorView) {
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length },
    userEvent: "delete",
  });
  view.focus();
}

function run(view: EditorView, action: MarkdownAction) {
  if (view.state.readOnly) return;
  switch (action) {
    case "bold":
    case "italic":
    case "strike":
    case "highlight":
      return wrap(view, markers[action][0], markers[action][1]);
    case "heading":
      return heading(view);
    case "bullet":
    case "ordered":
    case "quote":
      return prefixLines(view, action);
    case "link":
      return link(view, false);
    case "image":
      return link(view, true);
    case "code":
      return code(view);
    case "table":
      return insertBlock(
        view,
        "| 列1 | 列2 |\n| --- | --- |\n| 内容 | 内容 |",
        2,
        4,
      );
    case "rule":
      return insertBlock(view, "---");
    case "undo":
      undo(view);
      return view.focus();
    case "redo":
      redo(view);
      return view.focus();
    case "clear":
      return clear(view);
  }
}

const highlightStyle = HighlightStyle.define([
  // Only real headings: table header rows also carry the generic heading tag.
  {
    tag: [
      tags.heading1,
      tags.heading2,
      tags.heading3,
      tags.heading4,
      tags.heading5,
      tags.heading6,
    ],
    color: "var(--orange-dark)",
    fontWeight: "700",
  },
  { tag: tags.strong, color: "#15110d", fontWeight: "700" },
  { tag: tags.emphasis, fontStyle: "italic" },
  { tag: tags.strikethrough, textDecoration: "line-through" },
  {
    tag: tags.link,
    color: "#c2571f",
    textDecoration: "underline",
    textUnderlineOffset: "2px",
  },
  { tag: tags.url, color: "#a39b92", textDecoration: "none" },
  {
    tag: tags.monospace,
    color: "#b2460f",
    backgroundColor: "#fff1e7",
    borderRadius: "3px",
  },
  { tag: tags.quote, color: "#8a6a55" },
  {
    tag: [
      tags.processingInstruction,
      tags.contentSeparator,
      tags.labelName,
      tags.meta,
    ],
    color: "#c4b8ac",
  },
  { tag: tags.comment, color: "#a39b92" },
]);

// List markers stand out from the other, muted, Markdown symbols.
const listMarkers = new MatchDecorator({
  regexp: /^(\s*(?:>\s*)*)([-*+]|\d{1,9}[.)])(?=[ \t])/g,
  decorate(add, from, to, match) {
    add(from + match[1].length, to, Decoration.mark({ class: "cm-md-list" }));
  },
});
const listMarkerPlugin = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet;
    constructor(view: EditorView) {
      this.decorations = listMarkers.createDeco(view);
    }
    update(update: ViewUpdate) {
      this.decorations = listMarkers.updateDeco(update, this.decorations);
    }
  },
  { decorations: (plugin) => plugin.decorations },
);

const theme = EditorView.theme({
  "&": { height: "100%", backgroundColor: "#fff", color: "#3a3530" },
  "&.cm-focused": { outline: "none" },
  ".cm-scroller": {
    fontFamily: 'Consolas, "SFMono-Regular", "Microsoft YaHei", monospace',
    fontSize: "14px",
    lineHeight: "1.85",
  },
  ".cm-content": { padding: "10px 0", caretColor: "var(--orange-dark)" },
  ".cm-line": { padding: "0 16px" },
  ".cm-gutters": {
    backgroundColor: "#fbfaf8",
    color: "#bdb5ac",
    borderRight: "1px solid #f0ede8",
  },
  ".cm-lineNumbers .cm-gutterElement": {
    minWidth: "34px",
    padding: "0 10px 0 6px",
    fontSize: "12px",
  },
  ".cm-activeLine": { backgroundColor: "#fff8f2" },
  ".cm-activeLineGutter": {
    backgroundColor: "#fff3ea",
    color: "var(--orange-dark)",
    fontWeight: "700",
  },
  ".cm-placeholder": { color: "#a8a29a" },
  ".cm-md-list, .cm-md-list span": {
    color: "var(--orange)",
    fontWeight: "700",
  },
});

export default function MarkdownEditor({
  value,
  onChange,
  disabled = false,
  label = "文章 Markdown 正文",
  onReady,
  onHistory,
  syncScroll,
}: MarkdownEditorProps) {
  const host = useRef<HTMLDivElement>(null),
    view = useRef<EditorView | null>(null),
    editable = useRef(new Compartment()),
    // The latest callbacks, read by the editor's long-lived listeners.
    props = useRef({ onChange, onHistory });
  props.current = { onChange, onHistory };

  useEffect(() => {
    let undoable = false,
      redoable = false;
    const report = (state: EditorState) => {
      const canUndo = undoDepth(state) > 0,
        canRedo = redoDepth(state) > 0;
      if (canUndo === undoable && canRedo === redoable) return;
      undoable = canUndo;
      redoable = canRedo;
      props.current.onHistory?.(canUndo, canRedo);
    };
    const save = (target: EditorView) => {
      if (!target.state.readOnly) target.dom.closest("form")?.requestSubmit();
      return true;
    };
    const editor = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          highlightActiveLine(),
          highlightActiveLineGutter(),
          history(),
          EditorView.lineWrapping,
          new LanguageSupport(markdownLanguage),
          syntaxHighlighting(highlightStyle),
          listMarkerPlugin,
          placeholder("在这里输入正文。回车即换行，空一行即分段。"),
          EditorView.contentAttributes.of({ "aria-label": label }),
          editable.current.of(editableState(!disabled)),
          keymap.of([
            { key: "Mod-b", run: (v) => (run(v, "bold"), true) },
            { key: "Mod-i", run: (v) => (run(v, "italic"), true) },
            { key: "Mod-k", run: (v) => (run(v, "link"), true) },
            { key: "Mod-s", run: save, preventDefault: true },
            ...markdownKeymap,
            ...historyKeymap,
            ...defaultKeymap,
            indentWithTab,
          ]),
          theme,
          EditorView.updateListener.of((update) => {
            if (
              update.docChanged &&
              !update.transactions.some((tr) => tr.annotation(external))
            )
              props.current.onChange(update.state.doc.toString());
            report(update.state);
          }),
        ],
      }),
    });
    view.current = editor;
    onReady?.({
      run: (action) => run(editor, action),
      focus: () => editor.focus(),
    });
    return () => {
      view.current = null;
      editor.destroy();
    };
    // The editor is created once; later prop changes are applied below.
  }, []);

  // Text replaced from outside, such as the saved copy from the server.
  useEffect(() => {
    const editor = view.current;
    if (!editor) return;
    const current = editor.state.doc.toString();
    if (current !== value)
      editor.dispatch({
        changes: { from: 0, to: current.length, insert: value },
        annotations: [external.of(true), Transaction.addToHistory.of(false)],
      });
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({
      effects: editable.current.reconfigure(editableState(!disabled)),
    });
  }, [disabled]);

  // Compare mode: scrolling either side moves the other to the same place.
  useEffect(() => {
    const source = view.current?.scrollDOM,
      preview = syncScroll;
    if (!source || !preview) return;
    let leader: HTMLElement | null = null,
      timer = 0;
    const follow = (from: HTMLElement, to: HTMLElement) => () => {
      if (leader && leader !== from) return;
      leader = from;
      clearTimeout(timer);
      timer = window.setTimeout(() => (leader = null), 120);
      const range = from.scrollHeight - from.clientHeight;
      to.scrollTop =
        range > 0
          ? (from.scrollTop / range) * (to.scrollHeight - to.clientHeight)
          : 0;
    };
    const fromSource = follow(source, preview),
      fromPreview = follow(preview, source);
    source.addEventListener("scroll", fromSource, { passive: true });
    preview.addEventListener("scroll", fromPreview, { passive: true });
    return () => {
      clearTimeout(timer);
      source.removeEventListener("scroll", fromSource);
      preview.removeEventListener("scroll", fromPreview);
    };
  }, [syncScroll]);

  return <div className="markdown-cm" ref={host} />;
}
