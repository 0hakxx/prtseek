import assert from "node:assert/strict";
import test from "node:test";
import { executionLabel, outcomeLabel } from "./ctf-status.ts";

test("execution completion is separate from verified completion", () => {
  assert.equal(executionLabel("completed"), "실행 종료");
  assert.equal(outcomeLabel("unverified"), "목표 검증 대기");
  assert.equal(outcomeLabel("blocked"), "목표 막힘");
  assert.equal(executionLabel("interrupted"), "중단됨");
  assert.equal(executionLabel("timed_out"), "시간 초과됨");
});
