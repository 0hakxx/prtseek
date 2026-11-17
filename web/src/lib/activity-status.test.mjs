import { ENTRY_ROLES, entryRoleLabel } from "./entry-roles.ts";
import { activityElapsed, activityStatus } from "./tool-execution-state.ts";
import assert from "node:assert/strict";
import test from "node:test";

const step = (seq, kind, extra = {}) => ({
  seq,
  kind,
  worker: "mainagent",
  ts: "2026-11-17T06:00:00Z",
  summary: "",
  ...extra,
});

test("only solve and writeup are new entries; historical labels remain readable", () => {
  assert.deepEqual(
    ENTRY_ROLES.map(({ key, name }) => [key, name]),
    [
      ["solve", "解题"],
      ["writeup", "写 WP"],
    ],
  );
  for (const key of ["solve", "triage", "command", "solver"]) assert.equal(entryRoleLabel(key), "解题");
  assert.equal(entryRoleLabel("unknown-custom"), "unknown-custom");
});

test("server running with no new model activity remains model processing, without invented progress", () => {
  const status = activityStatus([step(1, "user")], true);
  assert.equal(status.kind, "model");
  assert.equal(status.updatedAt, "2026-11-17T06:00:00Z");
  assert.equal(activityStatus([], true).updatedAt, undefined);
});

test("tool activity waits for its matched result and resolves to model processing", () => {
  const call = step(2, "tool_use", {
    tool: "Bash",
    tool_use_id: "fixture",
    summary: 'Bash {"command":"python -c print(1)","description":"打印示例"}',
  });
  const steps = [step(1, "user"), call];
  assert.equal(activityStatus(steps, true).kind, "tool");
  assert.equal(activityStatus(steps, true).action, "打印示例");
  assert.equal(activityStatus([...steps, step(3, "tool_result", { tool_use_id: "fixture" })], true).kind, "model");
  assert.equal(activityStatus(steps, false).kind, "unreturned");
  assert.equal(activityStatus(steps, false, "interrupted").kind, "interrupted");
});

test("previous turns and inherited calls cannot become the current action", () => {
  const old = step(2, "tool_use", { tool: "Bash", tool_use_id: "old" });
  assert.equal(activityStatus([old, step(4, "user")], true).kind, "model");
  assert.equal(activityStatus([{ ...old, inherited: true }], true).kind, "model");
});

test("lost connection takes precedence over old running state and verified is distinct from ended", () => {
  assert.equal(activityStatus([], true, "running", "", true).kind, "disconnected");
  assert.equal(activityStatus([], false, "completed").label, "本轮已结束");
  assert.equal(activityStatus([], false, "completed", "verified").label, "目标已完成");
  assert.equal(activityStatus([], true, "", "", false, true).kind, "loading");
});

test("timers derive only from valid timestamps and never manufacture missing elapsed time", () => {
  assert.equal(activityElapsed("2026-11-17T06:00:00Z", Date.parse("2026-11-17T06:01:10Z")), 70000);
  assert.equal(activityElapsed(undefined, Date.now()), null);
  assert.equal(activityElapsed("invalid", Date.now()), null);
  assert.equal(activityElapsed("2026-11-17T06:00:00Z", 0), null);
});
