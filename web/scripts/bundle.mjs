// Bundle a TypeScript entry with esbuild in memory and import the result, so
// plain-Node scripts can use web/src without a build step or generated files.
import * as esbuild from "esbuild";
import { fileURLToPath } from "node:url";

const TARGET = "node20";

/**
 * @param {string | URL} entry absolute path or file URL of the entry module
 * @returns {Promise<Record<string, unknown>>} the bundled module's namespace
 */
export async function importBundled(entry) {
  const entryPath = entry instanceof URL ? fileURLToPath(entry) : entry;
  const result = await esbuild.build({
    entryPoints: [entryPath],
    bundle: true,
    write: false,
    format: "esm",
    platform: "node",
    target: TARGET,
    logLevel: "silent",
  });
  const file = result.outputFiles[0];
  if (file === undefined) throw new Error(`esbuild produced no output for ${entryPath}`);
  const url = `data:text/javascript;base64,${Buffer.from(file.text, "utf8").toString("base64")}`;
  return import(url);
}
