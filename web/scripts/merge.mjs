#!/usr/bin/env node
// Formatting-preserving merge from the command line:
//   node web/scripts/merge.mjs ORIGINAL.md NEW.md > merged.md
// Prints the merged Markdown to stdout and `preserved=true|false` to stderr.
// Uses the stand-alone remark parser (remark-parse + remark-gfm), which
// matches the editor's processor for everything the merge looks at.
import { readFileSync } from "node:fs";
import { importBundled } from "./bundle.mjs";

const USAGE = "usage: merge.mjs ORIGINAL NEW";
const EXIT_USAGE = 2;

const [originalPath, editedPath] = process.argv.slice(2);
if (originalPath === undefined || editedPath === undefined) {
  console.error(USAGE);
  process.exit(EXIT_USAGE);
}

const { mergeMarkdown, createParser } = await importBundled(new URL("../src/merge-tools.ts", import.meta.url));
const original = readFileSync(originalPath, "utf8");
const edited = readFileSync(editedPath, "utf8");
const result = mergeMarkdown(original, edited, createParser());
process.stdout.write(result.text);
console.error(`preserved=${result.preserved}`);
