import assert from "node:assert/strict";
import test from "node:test";

import { hideResearchEvidenceAnnotation } from "./research-evidence.ts";

test("hides only the internal research evidence annotation", () => {
  const annotation = "[연구 증거 evidence_id=t000002; research_record.entry.evidence_ids 인용 전용이며, 원래 도구의 업무 번호를 대신하지 않음] ";
  assert.equal(hideResearchEvidenceAnnotation(`${annotation}실제 도구 출력`), "실제 도구 출력");
  assert.equal(hideResearchEvidenceAnnotation(`첫 줄\n${annotation}두 번째 줄`), "첫 줄\n두 번째 줄");
  assert.equal(hideResearchEvidenceAnnotation("flag{fixture} evidence_id=t000002"), "flag{fixture} evidence_id=t000002");
});
