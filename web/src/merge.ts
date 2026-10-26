// Formatting-preserving merge of the editor's serialized Markdown back into
// the original source text. See DESIGN.md, "Formatting-preserving save".
//
// `O` is the file as loaded, `N` is the editor's serialization. Both are parsed
// with the same processor; blocks that are structurally unchanged are copied
// byte-for-byte from `O`, everything else comes from `N`. The result is
// validated by re-parsing: if it does not mean exactly what `N` means, `N` is
// returned verbatim. Pure: no DOM, no I/O.

import type { List, ListItem, Node, Parent, Root, RootContent } from "mdast";

/** Separator between root-level blocks that were not adjacent in `O`. */
const BLOCK_SEPARATOR = "\n\n";
const LINE_SEPARATOR = "\n";
const SPACE = " ";
const LIST_TYPE = "list";
const LIST_ITEM_TYPE = "listItem";
/** Node keys never compared (source offsets). */
const IGNORED_KEYS: ReadonlySet<string> = new Set(["position"]);
/** `listItem` keys derived from the item's index by Milkdown's remark plugins. */
const IGNORED_LIST_ITEM_KEYS: ReadonlySet<string> = new Set(["label"]);
/**
 * First line of a list item: marker, the spaces that set the content offset
 * (up to 4; more means indented code), and an optional GFM task box.
 */
const ITEM_PREFIX_RE = /^([-*+]|\d{1,9}[.)])( {0,4})(\[[ xX]\]( +|$|(?=\n)))?/;
const ORDERED_MARKER_RE = /^(\d{1,9})([.)])$/;
const BLANK_LINE_RE = /^[ \t]*$/;
const LEADING_SPACES_RE = /^ */;
const TASK_BOX_CHECKED = "[x]";
const TASK_BOX_UNCHECKED = "[ ]";
/** Myers diff gives up past this many edits and treats the middle as replaced. */
const MAX_DIFF_EDITS = 2048;
/** Weighted pairing of replaced children falls back to positional pairing past this DP size. */
const MAX_PAIRING_CELLS = 250_000;
/** Pairing scores: higher wins when aligning replaced children of the same type. */
const SCORE_NONE = 0;
const SCORE_SAME_TYPE = 1;
const SCORE_SAME_FIRST_CHILD = 2;
const SCORE_MERGEABLE = 3;
const SCORE_ONLY_CHECKED_DIFFERS = 4;

export type Parser = (markdown: string) => Root;

export interface MergeResult {
  text: string;
  /** False when validation failed; `text` is then exactly `edited`. */
  preserved: boolean;
}

/**
 * Merge `edited` (the editor's serialization) into `original` so that
 * untouched blocks keep their original bytes. Never throws.
 */
export function mergeMarkdown(original: string, edited: string, parse: Parser): MergeResult {
  if (original === edited) return { text: original, preserved: true };
  try {
    const oTree = parse(original);
    const nTree = parse(edited);
    const merger = new Merger(original, edited);
    const merged = merger.mergeRoot(oTree, nTree);
    if (merged === edited) return { text: edited, preserved: true };
    const check = parse(merged);
    if (hashOf(check) !== hashOf(nTree)) return { text: edited, preserved: false };
    return { text: merged, preserved: true };
  } catch {
    return { text: edited, preserved: false };
  }
}

/** Structural equality: same type, attributes and children; positions ignored. */
export function structurallyEqual(a: Node, b: Node): boolean {
  return hashOf(a) === hashOf(b);
}

// ---------------------------------------------------------------------------
// Structural hashing

const hashCache = new WeakMap<object, string>();

function isNodeLike(value: object): value is Node {
  return typeof (value as { type?: unknown }).type === "string";
}

function ignoredKey(key: string, type: string): boolean {
  if (IGNORED_KEYS.has(key)) return true;
  return type === LIST_ITEM_TYPE && IGNORED_LIST_ITEM_KEYS.has(key);
}

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value !== null && typeof value === "object") {
    if (isNodeLike(value)) return hashOf(value);
    return canonicalObject(value as Record<string, unknown>, "");
  }
  const json = JSON.stringify(value);
  return json === undefined ? "undefined" : json;
}

function canonicalObject(obj: Record<string, unknown>, type: string): string {
  const keys = Object.keys(obj)
    .filter((k) => obj[k] !== undefined && !ignoredKey(k, type))
    .sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(obj[k])}`).join(",")}}`;
}

/** Canonical string of a node with positions stripped; cached per node object. */
function hashOf(node: Node): string {
  const cached = hashCache.get(node);
  if (cached !== undefined) return cached;
  const hash = canonicalObject(node as unknown as Record<string, unknown>, node.type);
  hashCache.set(node, hash);
  return hash;
}

// ---------------------------------------------------------------------------
// Positions and slices

interface Span {
  start: number;
  end: number;
  /** 0-based column of the first character. */
  column: number;
}

function spanOf(node: Node): Span {
  const pos = node.position;
  if (pos === undefined) throw new Error(`node ${node.type} has no position`);
  const start = pos.start.offset;
  const end = pos.end.offset;
  if (start === undefined || end === undefined) throw new Error(`node ${node.type} has no offsets`);
  return { start, end, column: pos.start.column - 1 };
}

function childrenOf(node: Node): readonly Node[] {
  const children = (node as Partial<Parent>).children;
  return children === undefined ? [] : children;
}

function isList(node: Node): node is List {
  return node.type === LIST_TYPE;
}

function isListItem(node: Node): node is ListItem {
  return node.type === LIST_ITEM_TYPE;
}

function listAttrsEqual(a: List, b: List): boolean {
  return a.ordered === b.ordered && a.start === b.start && a.spread === b.spread;
}

function childrenEqual(a: Node, b: Node): boolean {
  const ac = childrenOf(a);
  const bc = childrenOf(b);
  if (ac.length !== bc.length) return false;
  for (let i = 0; i < ac.length; i++) {
    const x = ac[i];
    const y = bc[i];
    if (x === undefined || y === undefined || hashOf(x) !== hashOf(y)) return false;
  }
  return true;
}

function onlyCheckedDiffers(a: ListItem, b: ListItem): boolean {
  return a.checked !== b.checked && a.spread === b.spread && childrenEqual(a, b);
}

function firstChildEqual(a: Node, b: Node): boolean {
  const x = childrenOf(a)[0];
  const y = childrenOf(b)[0];
  return x !== undefined && y !== undefined && hashOf(x) === hashOf(y);
}

// ---------------------------------------------------------------------------
// Sequence alignment

type DiffOp = { kind: "equal"; a: number; b: number } | { kind: "delete"; a: number } | { kind: "insert"; b: number };

/** Myers O(ND) diff over hash strings; returns ops in order. */
function diffSequences(a: readonly string[], b: readonly string[]): DiffOp[] {
  let prefix = 0;
  while (prefix < a.length && prefix < b.length && a[prefix] === b[prefix]) prefix++;
  let suffix = 0;
  while (
    suffix < a.length - prefix &&
    suffix < b.length - prefix &&
    a[a.length - 1 - suffix] === b[b.length - 1 - suffix]
  ) {
    suffix++;
  }
  const ops: DiffOp[] = [];
  for (let i = 0; i < prefix; i++) ops.push({ kind: "equal", a: i, b: i });
  const midA = a.slice(prefix, a.length - suffix);
  const midB = b.slice(prefix, b.length - suffix);
  for (const op of myers(midA, midB)) {
    if (op.kind === "equal") ops.push({ kind: "equal", a: op.a + prefix, b: op.b + prefix });
    else if (op.kind === "delete") ops.push({ kind: "delete", a: op.a + prefix });
    else ops.push({ kind: "insert", b: op.b + prefix });
  }
  for (let i = 0; i < suffix; i++) {
    ops.push({ kind: "equal", a: a.length - suffix + i, b: b.length - suffix + i });
  }
  return ops;
}

function replaceAll(n: number, m: number): DiffOp[] {
  const ops: DiffOp[] = [];
  for (let i = 0; i < n; i++) ops.push({ kind: "delete", a: i });
  for (let j = 0; j < m; j++) ops.push({ kind: "insert", b: j });
  return ops;
}

function myers(a: readonly string[], b: readonly string[]): DiffOp[] {
  const n = a.length;
  const m = b.length;
  if (n === 0 || m === 0) return replaceAll(n, m);
  const max = Math.min(n + m, MAX_DIFF_EDITS);
  // v[k + offset] = furthest x on diagonal k. Before each edit distance d the
  // window k in [-d-1, d+1] is snapshotted for the backtrack (memory ~ D^2).
  const offset = max + 1;
  const v = new Int32Array(2 * offset + 1);
  const trace: Int32Array[] = [];
  let found = false;
  for (let d = 0; d <= max && !found; d++) {
    trace.push(v.slice(offset - d - 1, offset + d + 2));
    for (let k = -d; k <= d; k += 2) {
      const down = k === -d || (k !== d && v[offset + k - 1]! < v[offset + k + 1]!);
      let x = down ? v[offset + k + 1]! : v[offset + k - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        found = true;
        break;
      }
    }
  }
  if (!found) return replaceAll(n, m);
  const reversed: DiffOp[] = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d >= 0; d--) {
    const prev = trace[d]!;
    const base = d + 1; // prev[k + base] is v[k + offset] before step d
    const k = x - y;
    const down = k === -d || (k !== d && prev[base + k - 1]! < prev[base + k + 1]!);
    const prevK = down ? k + 1 : k - 1;
    const prevX = prev[base + prevK]!;
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      reversed.push({ kind: "equal", a: x - 1, b: y - 1 });
      x--;
      y--;
    }
    if (d > 0) {
      if (down) reversed.push({ kind: "insert", b: prevY });
      else reversed.push({ kind: "delete", a: prevX });
    }
    x = prevX;
    y = prevY;
  }
  return reversed.reverse();
}

/** One emitted child, in `N` order. `oIndex` is set when it reuses an `O` child. */
interface Aligned {
  nIndex: number;
  oIndex: number | null;
  equal: boolean;
}

/** Similarity of two unmatched children; 0 means they are not paired. */
function pairScore(o: Node, n: Node): number {
  if (o.type !== n.type) return SCORE_NONE;
  if (isListItem(o) && isListItem(n)) {
    if (onlyCheckedDiffers(o, n)) return SCORE_ONLY_CHECKED_DIFFERS;
    if (o.spread === n.spread && firstChildEqual(o, n)) return SCORE_SAME_FIRST_CHILD;
    return SCORE_SAME_TYPE;
  }
  if (isList(o) && isList(n)) return listAttrsEqual(o, n) ? SCORE_MERGEABLE : SCORE_SAME_TYPE;
  return SCORE_SAME_TYPE;
}

/** Order-preserving pairing of deleted `O` children with inserted `N` children. */
function pairRegion(oNodes: readonly Node[], oIdx: readonly number[], nNodes: readonly Node[], nIdx: readonly number[]): Map<number, number> {
  const pairs = new Map<number, number>();
  const rows = oIdx.length;
  const cols = nIdx.length;
  if (rows === 0 || cols === 0) return pairs;
  const scoreAt = (i: number, j: number): number => pairScore(oNodes[oIdx[i]!]!, nNodes[nIdx[j]!]!);
  if (rows * cols > MAX_PAIRING_CELLS) {
    for (let i = 0; i < Math.min(rows, cols); i++) {
      if (scoreAt(i, i) > SCORE_NONE) pairs.set(nIdx[i]!, oIdx[i]!);
    }
    return pairs;
  }
  // Weighted LCS: best[i][j] = max total score pairing oIdx[i..] with nIdx[j..].
  const width = cols + 1;
  const best = new Float64Array((rows + 1) * width);
  for (let i = rows - 1; i >= 0; i--) {
    for (let j = cols - 1; j >= 0; j--) {
      const skip = Math.max(best[(i + 1) * width + j]!, best[i * width + j + 1]!);
      const s = scoreAt(i, j);
      const take = s > SCORE_NONE ? s + best[(i + 1) * width + j + 1]! : SCORE_NONE;
      best[i * width + j] = Math.max(skip, take);
    }
  }
  let i = 0;
  let j = 0;
  while (i < rows && j < cols) {
    const here = best[i * width + j]!;
    const s = scoreAt(i, j);
    if (s > SCORE_NONE && here === s + best[(i + 1) * width + j + 1]!) {
      pairs.set(nIdx[j]!, oIdx[i]!);
      i++;
      j++;
    } else if (best[(i + 1) * width + j]! >= best[i * width + j + 1]!) {
      i++;
    } else {
      j++;
    }
  }
  return pairs;
}

/** Align children: exact matches by diff, then pair the leftovers by similarity. */
function alignChildren(oNodes: readonly Node[], nNodes: readonly Node[]): Aligned[] {
  const ops = diffSequences(oNodes.map(hashOf), nNodes.map(hashOf));
  const result: Aligned[] = [];
  let pendingO: number[] = [];
  let pendingN: number[] = [];
  const flush = (): void => {
    if (pendingN.length === 0) {
      pendingO = [];
      return;
    }
    const pairs = pairRegion(oNodes, pendingO, nNodes, pendingN);
    for (const nIndex of pendingN) {
      const oIndex = pairs.get(nIndex);
      result.push({ nIndex, oIndex: oIndex === undefined ? null : oIndex, equal: false });
    }
    pendingO = [];
    pendingN = [];
  };
  for (const op of ops) {
    if (op.kind === "equal") {
      flush();
      result.push({ nIndex: op.b, oIndex: op.a, equal: true });
    } else if (op.kind === "delete") {
      pendingO.push(op.a);
    } else {
      pendingN.push(op.b);
    }
  }
  flush();
  return result;
}

// ---------------------------------------------------------------------------
// List item text surgery

interface ItemPrefix {
  marker: string;
  /** Spaces after the marker; empty when the content starts on the next line. */
  spaces: string;
  /** Task box with its trailing spaces, e.g. `[ ] `, or empty. */
  box: string;
  /** Length of the whole prefix in the first line. */
  length: number;
}

function parseItemPrefix(text: string): ItemPrefix {
  const m = ITEM_PREFIX_RE.exec(text);
  if (m === null) throw new Error(`not a list item: ${JSON.stringify(text.slice(0, 20))}`);
  return { marker: m[1]!, spaces: m[2]!, box: m[3] ?? "", length: m[0].length };
}

/** Column (relative to the marker) where the item's content lines are indented. */
function contentWidth(prefix: ItemPrefix): number {
  return prefix.marker.length + Math.max(prefix.spaces.length, 1);
}

/** Marker style copied from an `O` item: marker text, spacing and absolute column. */
interface ItemStyle {
  marker: string;
  spaces: string;
  column: number;
}

function styleOf(prefix: ItemPrefix, column: number): ItemStyle {
  return { marker: prefix.marker, spaces: prefix.spaces === "" ? SPACE : prefix.spaces, column };
}

function boxText(checked: boolean): string {
  return checked ? TASK_BOX_CHECKED : TASK_BOX_UNCHECKED;
}

/** Re-indent continuation lines (every line after the first) from one content column to another. */
function reindent(text: string, from: number, to: number): string {
  if (from === to || !text.includes(LINE_SEPARATOR)) return text;
  const lines = text.split(LINE_SEPARATOR);
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i]!;
    if (BLANK_LINE_RE.test(line)) continue;
    const leading = LEADING_SPACES_RE.exec(line)![0].length;
    if (leading >= from) lines[i] = SPACE.repeat(to) + line.slice(from);
  }
  return lines.join(LINE_SEPARATOR);
}

/**
 * Rewrite an item's first-line marker to `style` and re-indent its body.
 * `checked`: `null` removes any task box, a boolean sets it, `undefined` keeps
 * whatever the text has.
 */
function restyleItem(text: string, sourceColumn: number, style: ItemStyle, checked: boolean | null | undefined): string {
  const prefix = parseItemPrefix(text);
  const rest = text.slice(prefix.length);
  let box: string;
  if (checked === undefined) box = prefix.box;
  else if (checked === null) box = "";
  else box = boxText(checked) + (prefix.box === "" ? SPACE : prefix.box.slice(TASK_BOX_CHECKED.length));
  const from = sourceColumn + contentWidth(prefix);
  const to = style.column + style.marker.length + style.spaces.length;
  const body = reindent(rest, from, to);
  let out = style.marker;
  if (box !== "" || (body !== "" && !body.startsWith(LINE_SEPARATOR))) out += style.spaces;
  out += body === "" ? box.trimEnd() : box + body;
  return out;
}

function orderedNumber(marker: string): number | null {
  const m = ORDERED_MARKER_RE.exec(marker);
  return m === null ? null : Number(m[1]);
}

function withNumber(style: ItemStyle, number: number): ItemStyle {
  const m = ORDERED_MARKER_RE.exec(style.marker);
  if (m === null) return style;
  return { ...style, marker: `${number}${m[2]!}` };
}

// ---------------------------------------------------------------------------
// The merge

/** Text emitted for one aligned child, with what it reused from `O`. */
interface Emitted {
  text: string;
  oIndex: number | null;
  /** Column of the marker in the text's source document (for list items). */
  sourceColumn: number;
  fromN: boolean;
}

class Merger {
  private readonly o: string;
  private readonly n: string;

  constructor(original: string, edited: string) {
    this.o = original;
    this.n = edited;
  }

  private oSlice(node: Node): string {
    const s = spanOf(node);
    return this.o.slice(s.start, s.end);
  }

  private nSlice(node: Node): string {
    const s = spanOf(node);
    return this.n.slice(s.start, s.end);
  }

  mergeRoot(oRoot: Root, nRoot: Root): string {
    if (hashOf(oRoot) === hashOf(nRoot)) return this.o;
    const oChildren: readonly RootContent[] = oRoot.children;
    const nChildren: readonly RootContent[] = nRoot.children;
    const emitted = this.emitChildren(oChildren, nChildren, 0, 0);
    const first = oChildren[0];
    const last = oChildren[oChildren.length - 1];
    const leading = first === undefined ? "" : this.o.slice(0, spanOf(first).start);
    let trailing: string;
    if (last !== undefined) trailing = this.o.slice(spanOf(last).end);
    else {
      const nLast = nChildren[nChildren.length - 1];
      trailing = nLast === undefined ? this.n : this.n.slice(spanOf(nLast).end);
    }
    return leading + this.joinEmitted(emitted, oChildren, BLOCK_SEPARATOR) + trailing;
  }

  /** Merge one `N` child against the `O` child it was paired with. */
  private mergeNode(oNode: Node, nNode: Node, oIndent: number, nIndent: number): string {
    if (hashOf(oNode) === hashOf(nNode)) return this.oSlice(oNode);
    if (isList(oNode) && isList(nNode) && listAttrsEqual(oNode, nNode)) return this.mergeList(oNode, nNode);
    return reindent(this.nSlice(nNode), nIndent, oIndent);
  }

  /** Emit aligned children of a block container (root or list item). */
  private emitChildren(oNodes: readonly Node[], nNodes: readonly Node[], oIndent: number, nIndent: number): Emitted[] {
    const out: Emitted[] = [];
    for (const entry of alignChildren(oNodes, nNodes)) {
      const nNode = nNodes[entry.nIndex]!;
      if (entry.oIndex === null) {
        out.push({ text: reindent(this.nSlice(nNode), nIndent, oIndent), oIndex: null, sourceColumn: spanOf(nNode).column, fromN: true });
        continue;
      }
      const oNode = oNodes[entry.oIndex]!;
      const text = entry.equal ? this.oSlice(oNode) : this.mergeNode(oNode, nNode, oIndent, nIndent);
      out.push({ text, oIndex: entry.oIndex, sourceColumn: spanOf(oNode).column, fromN: false });
    }
    return out;
  }

  /** Join emitted children, reusing `O`'s gap text between children adjacent in `O`. */
  private joinEmitted(emitted: readonly Emitted[], oNodes: readonly Node[], separator: string): string {
    let text = "";
    let prev: Emitted | undefined;
    for (const item of emitted) {
      if (prev !== undefined) {
        if (prev.oIndex !== null && item.oIndex === prev.oIndex + 1) {
          text += this.o.slice(spanOf(oNodes[prev.oIndex]!).end, spanOf(oNodes[item.oIndex]!).start);
        } else {
          text += separator;
        }
      }
      text += item.text;
      prev = item;
    }
    return text;
  }

  private mergeList(oList: List, nList: List): string {
    const oItems: readonly ListItem[] = oList.children;
    const nItems: readonly ListItem[] = nList.children;
    const emitted: Emitted[] = [];
    for (const entry of alignChildren(oItems, nItems)) {
      const nItem = nItems[entry.nIndex]!;
      if (entry.oIndex === null) {
        emitted.push({ text: this.nSlice(nItem), oIndex: null, sourceColumn: spanOf(nItem).column, fromN: true });
        continue;
      }
      const oItem = oItems[entry.oIndex]!;
      let text: string;
      if (entry.equal) text = this.oSlice(oItem);
      else if (oItem.spread === nItem.spread) text = this.mergeListItem(oItem, nItem);
      else text = restyleItem(this.nSlice(nItem), spanOf(nItem).column, this.styleOfItem(oItem), nItem.checked ?? null);
      emitted.push({ text, oIndex: entry.oIndex, sourceColumn: spanOf(oItem).column, fromN: false });
    }
    this.restyleItems(emitted, oItems, nItems, nList);
    const separator = (nList.spread ? BLOCK_SEPARATOR : LINE_SEPARATOR) + SPACE.repeat(this.listColumn(emitted, oItems));
    return this.joinEmitted(emitted, oItems, separator);
  }

  private styleOfItem(oItem: ListItem): ItemStyle {
    const span = spanOf(oItem);
    return styleOf(parseItemPrefix(this.oSlice(oItem)), span.column);
  }

  /** Column of the `O` list's markers (from its first item). */
  private listColumn(emitted: readonly Emitted[], oItems: readonly ListItem[]): number {
    for (const e of emitted) if (e.oIndex !== null) return spanOf(oItems[e.oIndex]!).column;
    return spanOf(oItems[0]!).column;
  }

  /** Give `N`-sourced items the `O` marker style; keep `O`'s numbering scheme. */
  private restyleItems(emitted: Emitted[], oItems: readonly ListItem[], nItems: readonly ListItem[], nList: List): void {
    const oNumbers = oItems.map((item) => orderedNumber(parseItemPrefix(this.oSlice(item)).marker));
    const firstNumber = oNumbers[0] ?? null;
    const ordered = nList.ordered === true && firstNumber !== null;
    const allSame = ordered && oNumbers.length >= 2 && oNumbers.every((x) => x === firstNumber);
    const start = nList.start ?? 1;
    const expectedNumber = (index: number): number => (allSame ? firstNumber! : start + index);
    const changed = emitted.some((e) => e.fromN) || emitted.filter((e) => e.oIndex !== null).length !== oItems.length;
    for (let i = 0; i < emitted.length; i++) {
      const e = emitted[i]!;
      if (e.fromN) {
        const neighbour = this.nearestOItem(emitted, i, oItems);
        let style = this.styleOfItem(neighbour);
        if (ordered) style = withNumber(style, expectedNumber(i));
        const nItem = nItems[i]!;
        emitted[i] = { ...e, text: restyleItem(e.text, e.sourceColumn, style, nItem.checked ?? null), sourceColumn: style.column };
      } else if (ordered && changed) {
        const prefix = parseItemPrefix(e.text);
        const current = orderedNumber(prefix.marker);
        const expected = expectedNumber(i);
        if (current !== null && current !== expected) {
          const style = withNumber(styleOf(prefix, e.sourceColumn), expected);
          emitted[i] = { ...e, text: restyleItem(e.text, e.sourceColumn, style, undefined) };
        }
      }
    }
  }

  private nearestOItem(emitted: readonly Emitted[], index: number, oItems: readonly ListItem[]): ListItem {
    for (let i = index - 1; i >= 0; i--) {
      const oIndex = emitted[i]!.oIndex;
      if (oIndex !== null) return oItems[oIndex]!;
    }
    for (let i = index + 1; i < emitted.length; i++) {
      const oIndex = emitted[i]!.oIndex;
      if (oIndex !== null) return oItems[oIndex]!;
    }
    return oItems[0]!;
  }

  /** Merge two list items with equal `spread`: `O` prefix (box from `N`), then aligned children. */
  private mergeListItem(oItem: ListItem, nItem: ListItem): string {
    const oText = this.oSlice(oItem);
    const oPrefix = parseItemPrefix(oText);
    const oSpan = spanOf(oItem);
    const nSpan = spanOf(nItem);
    const nPrefix = parseItemPrefix(this.nSlice(nItem));
    const oContent = oSpan.column + contentWidth(oPrefix);
    const nContent = nSpan.column + contentWidth(nPrefix);
    const checked = nItem.checked ?? null;
    let box = "";
    if (checked !== null) box = boxText(checked) + (oPrefix.box === "" ? SPACE : oPrefix.box.slice(TASK_BOX_CHECKED.length));
    const spaces = oPrefix.spaces === "" ? SPACE : oPrefix.spaces;
    const emitted = this.emitChildren(oItem.children, nItem.children, oContent, nContent);
    if (emitted.length === 0) return oPrefix.marker + (box === "" ? "" : spaces + box.trimEnd());
    const separator = (nItem.spread ? BLOCK_SEPARATOR : LINE_SEPARATOR) + SPACE.repeat(oContent);
    return oPrefix.marker + spaces + box + this.joinEmitted(emitted, oItem.children, separator);
  }
}

/** Exposed for unit tests only. */
export const MERGE_INTERNALS = { diffSequences, alignChildren, reindent, restyleItem, parseItemPrefix };
