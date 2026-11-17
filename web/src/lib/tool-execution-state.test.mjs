import { createToolExecutionContext, toolExecutionState } from "./tool-execution-state.ts";
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
const state = (use, result, activity, live) =>
  toolExecutionState(use, result, createToolExecutionContext(activity, live));

test("a process restart leaves an unmatched call unreturned instead of running or failed", () => {
  const use = step(12, "tool_use");
  assert.equal(state(use, undefined, [step(10, "user"), use], false), "unreturned");
  assert.equal(state(use, undefined, [use], undefined), "unreturned");
});

test("only unmatched tools in the live turn show as running", () => {
  const old = step(12, "tool_use");
  const current = step(23, "tool_use");
  const activity = [step(10, "user"), old, step(20, "user"), current];
  assert.equal(state(old, undefined, activity, true), "unreturned");
  assert.equal(state(current, undefined, activity, true), "running");
});

test("a terminal result closes older unmatched calls even while live polling is catching up", () => {
  const old = step(12, "tool_use");
  const terminal = step(14, "result", { is_error: true });
  assert.equal(state(old, undefined, [old, terminal], true), "unreturned");
  const next = step(16, "tool_use");
  assert.equal(state(next, undefined, [old, terminal, next], true), "running");
});

test("one parallel worker's terminal does not mark another live worker's tool interrupted", () => {
  const first = step(11, "tool_use", { worker: "work#1" });
  const second = step(12, "tool_use", { worker: "work#2" });
  const activity = [first, second, step(13, "result", { worker: "work#1" })];
  assert.equal(state(first, undefined, activity, true), "unreturned");
  assert.equal(state(second, undefined, activity, true), "running");
});

test("round and worker intent boundaries do not revive tools from a previous assignment", () => {
  const old = step(11, "tool_use", { worker: "work#1" });
  assert.equal(state(old, undefined, [old, step(12, "round", { worker: "planner" })], true), "unreturned");
  assert.equal(state(old, undefined, [old, step(12, "intent", { worker: "work#1" })], true), "unreturned");
});

test("actual results determine success and failure even after a later turn starts", () => {
  const use = step(12, "tool_use");
  const result = step(13, "tool_result");
  const activity = [use, result, step(20, "user")];
  assert.equal(state(use, result, activity, true), "succeeded");
  assert.equal(state(use, { ...result, is_error: true }, activity, false), "failed");
  assert.equal(state(undefined, result, activity, false), "succeeded");
});

test("inherited history never becomes live and its foreign seqs do not close local work", () => {
  const inherited = step(999, "tool_use", { inherited: true });
  const local = step(15, "tool_use");
  const activity = [step(1000, "user", { inherited: true }), inherited, step(10, "user"), local];
  assert.equal(state(inherited, undefined, activity, true), "unreturned");
  assert.equal(state(local, undefined, activity, true), "running");
});

test("overlapping out-of-order history keeps the newest boundary without mutating input", () => {
  const use = Object.freeze(step(12, "tool_use"));
  const activity = Object.freeze([step(20, "user"), use, step(10, "user"), step(20, "user")]);
  assert.equal(state(use, undefined, activity, true), "unreturned");
  assert.deepEqual(
    activity.map((entry) => entry.seq),
    [20, 12, 10, 20],
  );
});
