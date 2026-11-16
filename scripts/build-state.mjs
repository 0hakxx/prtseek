import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, lstatSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const projectRoot = fileURLToPath(new URL("../", import.meta.url));
const sourceRoots = ["agent", "cmd", "config", "db", "enrich", "evidence", "guard", "intercept", "llmpool", "llmrec", "mcphttp", "notify", "report", "selfupdate", "server", "sidequestion", "traffic", "skills", "scripts", "web/src", "web/public", "web/scripts"];
const rootFiles = ["VERSION", "go.mod", "go.sum", "build.sh", "build-prtseek.ps1", "start.sh", "start-prtseek.ps1", "update.sh", "Dockerfile", "web/package.json", "web/package-lock.json", "web/next.config.mjs", "web/tsconfig.json", "web/postcss.config.mjs"];
function digestFile(filename) { return createHash("sha256").update(readFileSync(filename)).digest("hex"); }
export function buildState(root = projectRoot) {
  const files = [];
  function visit(relative) {
    const full = path.join(root, relative);
    if (!existsSync(full)) return;
    const entry = lstatSync(full);
    if (entry.isSymbolicLink()) throw new Error(`Build input cannot be a symlink: ${relative}`);
    if (entry.isDirectory()) {
      for (const name of readdirSync(full).sort()) {
        if (["dist", "node_modules", ".next", "__pycache__"].includes(name)) continue;
        visit(`${relative}/${name}`);
      }
    } else if (entry.isFile() && relative !== "web/public/build-info.json" && !relative.endsWith(".pyc")) files.push(relative);
  }
  for (const root of [...sourceRoots, ...rootFiles]) visit(root);
  const hash = createHash("sha256");
  for (const name of files.sort()) hash.update(name).update("\0").update(readFileSync(path.join(root, name))).update("\0");
  return { version: readFileSync(path.join(root, "VERSION"), "utf8").trim(), source_id: hash.digest("hex") };
}
export function recordBuild(binary, root = projectRoot) {
  const state = { ...buildState(root), binary_sha256: digestFile(binary) };
  writeFileSync(`${binary}.build.json`, JSON.stringify(state) + "\n");
  return state;
}
export function isCurrent(binary, root = projectRoot) {
  try {
    const stored = JSON.parse(readFileSync(`${binary}.build.json`, "utf8"));
    return stored.source_id === buildState(root).source_id && stored.binary_sha256 === digestFile(binary);
  } catch { return false; }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [command, binary] = process.argv.slice(2);
  if (command === "check") process.exitCode = isCurrent(binary) ? 0 : 1;
  else if (command === "write") {
    const ui = JSON.parse(readFileSync(path.join(projectRoot, "server/webui/dist/build-info.json"), "utf8"));
    if (ui.source_id !== buildState().source_id) throw new Error("Embedded UI is stale or source changed during build. Rebuild the frontend before installing.");
    console.log(JSON.stringify(recordBuild(binary)));
  }
  else if (command === "version") console.log(buildState().version);
  else if (command === "id") console.log(buildState().source_id);
  else console.log(JSON.stringify(buildState()));
}
