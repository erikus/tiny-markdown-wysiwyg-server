// Stand-alone remark pipeline (parse + GFM, stringify with the editor's
// options) for the merge CLI and tests. The editor itself uses Milkdown's
// processor; see editor.ts.

import type { Root } from "mdast";
import remarkGfm from "remark-gfm";
import remarkParse from "remark-parse";
import remarkStringify from "remark-stringify";
import { unified } from "unified";
import type { Parser } from "./merge";
import { EDITOR_STRINGIFY_OPTIONS } from "./stringify-options";

/** Markdown → mdast with GFM, keeping positions. */
export function createParser(): Parser {
  const processor = unified().use(remarkParse).use(remarkGfm);
  return (markdown: string): Root => processor.runSync(processor.parse(markdown), markdown) as Root;
}

/** mdast → Markdown with the editor's stringify options. */
export function createSerializer(): (tree: Root) => string {
  const processor = unified().use(remarkGfm).use(remarkStringify, EDITOR_STRINGIFY_OPTIONS);
  return (tree: Root): string => processor.stringify(tree);
}
