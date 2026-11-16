import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { buildState, recordBuild, isCurrent } from "./build-state.mjs";

test("source changes, missing manifests and replaced executables cannot reuse an old build", () => {
  const root = mkdtempSync(path.join(tmpdir(), "prtseek-build-"));
  try {
    mkdirSync(path.join(root, "agent"));
    mkdirSync(path.join(root, "data"));
    writeFileSync(path.join(root, "VERSION"), "prtseek-test\n");
    const source = path.join(root, "agent/chat.go"), binary = path.join(root, "prtseek");
    writeFileSync(source, "source-v1"); writeFileSync(binary, "binary-v1");
    assert.equal(isCurrent(binary, root), false);
    recordBuild(binary, root);
    assert.equal(isCurrent(binary, root), true);
    const first = buildState(root).source_id;
    writeFileSync(path.join(root, "data/state.db"), "private state");
    assert.equal(buildState(root).source_id, first);
    writeFileSync(source, "source-v2");
    assert.equal(isCurrent(binary, root), false);
    recordBuild(binary, root);
    writeFileSync(binary, "replaced binary");
    assert.equal(isCurrent(binary, root), false);
  } finally { rmSync(root, { recursive: true, force: true }); }
});
