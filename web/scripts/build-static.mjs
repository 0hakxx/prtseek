import { spawnSync } from "node:child_process";
import { copyFileSync, readdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildState } from "../../scripts/build-state.mjs";

const state = buildState();
writeFileSync(new URL("../public/build-info.json", import.meta.url), JSON.stringify(state) + "\n");

const result = spawnSync(process.execPath, ["node_modules/next/dist/bin/next", "build"], {
  stdio: "inherit",
  env: { ...process.env, NEXT_EXPORT: "1", NEXT_PUBLIC_BUILD_ID: state.source_id },
});
if (result.error) console.error(result.error.message);
if (result.status !== 0) process.exit(result.status ?? 1);

// Next 16 exports nested segment payloads, while its static client requests
// dotted filenames. Materialize those aliases for Go and other static hosts.
const out = fileURLToPath(new URL("../out/", import.meta.url));
function segmentAliases(directory) {
  let count = 0;
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const source = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      count += segmentAliases(source);
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith(".txt")) continue;
    const parts = path.relative(out, source).split(path.sep);
    const segment = parts.findIndex((part) => part.startsWith("__next."));
    if (segment < 0 || segment === parts.length - 1) continue;
    const alias = path.join(out, ...parts.slice(0, segment), parts.slice(segment).join("."));
    copyFileSync(source, alias);
    count++;
  }
  return count;
}
console.log(`Static segment aliases: ${segmentAliases(out)}`);
