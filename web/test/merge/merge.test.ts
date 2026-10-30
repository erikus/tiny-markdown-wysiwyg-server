// Tests for the formatting-preserving merge (src/merge.ts). Bundled by
// test/run.mjs and run with `node --test`; the corpus directory arrives in
// MDWEBD_MERGE_CORPUS.
//
// Corpus files come in triples: NAME.original.md (hand-written), NAME.edited.md
// (the edit as the serializer would emit it) and NAME.expected.md (the merge
// result). Every original is also checked for stability and every pair for
// correctness, as DESIGN.md requires.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { MERGE_INTERNALS, mergeMarkdown, structurallyEqual } from "../../src/merge";
import { createParser, createSerializer } from "../../src/remark";

const CORPUS_ENV = "MDWEBD_MERGE_CORPUS";
const ORIGINAL_SUFFIX = ".original.md";
const EDITED_SUFFIX = ".edited.md";
const EXPECTED_SUFFIX = ".expected.md";
/** Fixture kinds the spec requires; each must have a triple in the corpus. */
const REQUIRED_CASES: readonly string[] = [
  "bullets-toggle",
  "nested-2space",
  "nested-4space",
  "ordered-same",
  "ordered-incrementing",
  "table-untouched",
  "thematic-breaks",
  "escapes",
  "blockquote",
  "fenced-code",
  "insert-paragraph",
  "list-insert-delete",
  "heading-changed",
];

const corpusDir: string | undefined = process.env[CORPUS_ENV];
if (corpusDir === undefined) throw new Error(`${CORPUS_ENV} is not set; run via test/run.mjs`);
const corpus: string = corpusDir;

const parse = createParser();
const serialize = createSerializer();

function read(name: string): string {
  return readFileSync(join(corpus, name), "utf8");
}

const caseNames: string[] = readdirSync(corpus)
  .filter((f) => f.endsWith(ORIGINAL_SUFFIX))
  .map((f) => f.slice(0, -ORIGINAL_SUFFIX.length))
  .sort();

test("corpus covers every required case", () => {
  for (const name of REQUIRED_CASES) {
    assert.ok(caseNames.includes(name), `missing corpus case ${name}`);
    assert.ok(readdirSync(corpus).includes(`${name}${EDITED_SUFFIX}`), `missing ${name}${EDITED_SUFFIX}`);
    assert.ok(readdirSync(corpus).includes(`${name}${EXPECTED_SUFFIX}`), `missing ${name}${EXPECTED_SUFFIX}`);
  }
});

for (const name of caseNames) {
  const original = read(`${name}${ORIGINAL_SUFFIX}`);

  test(`stability: ${name}`, () => {
    const reserialized = serialize(parse(original));
    const result = mergeMarkdown(original, reserialized, parse);
    assert.equal(result.preserved, true);
    assert.equal(result.text, original);
  });

  const files = readdirSync(corpus);
  if (!files.includes(`${name}${EDITED_SUFFIX}`)) continue;
  const edited = read(`${name}${EDITED_SUFFIX}`);
  const expected = read(`${name}${EXPECTED_SUFFIX}`);

  test(`fixture is serializer output: ${name}`, () => {
    assert.equal(serialize(parse(edited)), edited);
  });

  test(`correctness: ${name}`, () => {
    const result = mergeMarkdown(original, edited, parse);
    assert.ok(structurallyEqual(parse(result.text), parse(edited)), "merged text must parse like the edit");
  });

  test(`expected output: ${name}`, () => {
    const result = mergeMarkdown(original, edited, parse);
    assert.equal(result.preserved, true);
    assert.equal(result.text, expected);
  });
}

test("identical input is returned untouched", () => {
  const text = "* a\n* b\n";
  assert.deepEqual(mergeMarkdown(text, text, parse), { text, preserved: true });
});

test("unmergeable edit falls back to the edited text", () => {
  // A tight paragraph + list; the edit makes the list start at 2. Reusing the
  // original "\n" gap would yield "intro\n2. a\n3. b", which is one paragraph
  // (an ordered list must start at 1 to interrupt a paragraph), so validation
  // rejects the merge.
  const original = "intro\n1. a\n2. b\n";
  const edited = serialize(parse("intro\n\n2. a\n3. b\n"));
  const result = mergeMarkdown(original, edited, parse);
  assert.equal(result.preserved, false);
  assert.equal(result.text, edited);
});

test("a throwing parser falls back", () => {
  const boom = (): never => {
    throw new Error("boom");
  };
  assert.deepEqual(mergeMarkdown("a\n", "b\n", boom), { text: "b\n", preserved: false });
});

test("structural equality ignores positions", () => {
  const a = parse("- [ ] x\n");
  const b = parse("\n\n- [ ] x\n");
  assert.ok(structurallyEqual(a, b));
  assert.ok(!structurallyEqual(a, parse("- [x] x\n")));
});

test("diffSequences aligns common prefix, middle and suffix", () => {
  const ops = MERGE_INTERNALS.diffSequences(["a", "b", "c", "d"], ["a", "x", "c", "d"]);
  assert.deepEqual(ops, [
    { kind: "equal", a: 0, b: 0 },
    { kind: "delete", a: 1 },
    { kind: "insert", b: 1 },
    { kind: "equal", a: 2, b: 2 },
    { kind: "equal", a: 3, b: 3 },
  ]);
});

test("reindent moves continuation lines between content offsets", () => {
  assert.equal(MERGE_INTERNALS.reindent("first\n  second\n\n  third", 2, 4), "first\n    second\n\n    third");
  assert.equal(MERGE_INTERNALS.reindent("first\n  second", 2, 2), "first\n  second");
});

test("restyleItem rewrites the marker, box and indentation", () => {
  const style = { marker: "*", spaces: "   ", column: 0 };
  assert.equal(MERGE_INTERNALS.restyleItem("- [ ] text\n  more", 0, style, true), "*   [x] text\n    more");
  assert.equal(MERGE_INTERNALS.restyleItem("1. [x] text", 0, { marker: "1)", spaces: " ", column: 0 }, null), "1) text");
  assert.equal(MERGE_INTERNALS.restyleItem("- [ ]", 0, { marker: "-", spaces: " ", column: 0 }, undefined), "- [ ]");
});

test("parseItemPrefix handles bullets, numbers and boxes", () => {
  assert.deepEqual(MERGE_INTERNALS.parseItemPrefix("- [x] foo"), { marker: "-", spaces: " ", box: "[x] ", length: 6 });
  assert.deepEqual(MERGE_INTERNALS.parseItemPrefix("10) foo"), { marker: "10)", spaces: " ", box: "", length: 4 });
  assert.deepEqual(MERGE_INTERNALS.parseItemPrefix("-\n  foo"), { marker: "-", spaces: "", box: "", length: 1 });
});
