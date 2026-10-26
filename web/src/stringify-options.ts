// remark-stringify options shared by the editor and the merge tooling so new
// content follows the most common hand-written style (DESIGN.md).

import type { Options } from "remark-stringify";

export const EDITOR_STRINGIFY_OPTIONS: Readonly<Options> = {
  bullet: "-",
  rule: "-",
  listItemIndent: "one",
  fences: true,
  emphasis: "_",
  strong: "*",
};
