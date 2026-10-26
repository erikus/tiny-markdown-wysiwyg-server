// Bundle entry for the Node-side tooling (scripts/merge.mjs): the merge plus
// a stand-alone parser/serializer.

export { mergeMarkdown, structurallyEqual, MERGE_INTERNALS } from "./merge";
export type { MergeResult, Parser } from "./merge";
export { createParser, createSerializer } from "./remark";
