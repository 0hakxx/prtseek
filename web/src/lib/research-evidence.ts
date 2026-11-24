// The research runtime gives the model an internal observation ID so it can
// reference real evidence. This annotation is not part of the operator-facing
// tool output, including when an older conversation is replayed.
const internalEvidenceAnnotation = /\[연구 증거 evidence_id=t\d+; research_record\.entry\.evidence_ids 인용 전용이며, 원래 도구의 업무 번호를 대신하지 않음\]\s*/g;

export function hideResearchEvidenceAnnotation(value: string): string {
  return value.replace(internalEvidenceAnnotation, "");
}
