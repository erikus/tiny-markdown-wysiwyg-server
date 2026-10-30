// Test runner: esbuild-bundle test/**/*.test.ts into a temp dir, run node --test.
import * as esbuild from "esbuild";
import { globSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const TEST_DIR = dirname(fileURLToPath(import.meta.url));
const WEB_DIR = resolve(TEST_DIR, "..");
const TEST_GLOB = "**/*.test.ts";
const CORPUS_DIR = join(TEST_DIR, "merge", "corpus");
/** Env var the tests read to find the fixture corpus. */
const CORPUS_ENV = "MDWEBD_MERGE_CORPUS";
const TARGET = "node20";

const entryPoints = globSync(TEST_GLOB, { cwd: TEST_DIR }).map((p) => join(TEST_DIR, p));
if (entryPoints.length === 0) {
  console.error(`no tests matching ${TEST_GLOB} under ${TEST_DIR}`);
  process.exit(1);
}

const outDir = mkdtempSync(join(tmpdir(), "mdwebd-web-test-"));
try {
  await esbuild.build({
    entryPoints,
    bundle: true,
    outdir: outDir,
    format: "esm",
    platform: "node",
    target: TARGET,
    outExtension: { ".js": ".mjs" },
    logLevel: "warning",
  });
  const bundled = entryPoints.map((p) => join(outDir, basename(p).replace(/\.ts$/, ".mjs")));
  const result = spawnSync(process.execPath, ["--test", ...bundled], {
    cwd: WEB_DIR,
    stdio: "inherit",
    env: { ...process.env, [CORPUS_ENV]: CORPUS_DIR },
  });
  process.exitCode = result.status ?? 1;
} finally {
  rmSync(outDir, { recursive: true, force: true });
}
