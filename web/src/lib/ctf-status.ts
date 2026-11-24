export function executionLabel(state: string, running = false): string {
  if (running || state === "running") return "실행 중";
  if (state === "blocked") return "조건 대기";
  if (state === "budget_exhausted") return "예산 소진";
  if (state === "stalled") return "진행 정체";
  return ({ completed: "실행 종료", cancelled: "취소됨", interrupted: "중단됨", timed_out: "시간 초과됨", failed: "실행 실패", idle: "대기" } as Record<string, string>)[state] ?? "상태 알 수 없음";
}

export function outcomeLabel(state: string): string {
  return ({ verified: "검증됨 · Agent 보고", partial: "부분 진전", blocked: "목표 막힘", unverified: "목표 검증 대기" } as Record<string, string>)[state] ?? "목표 검증 대기";
}

export function elapsedLabel(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  return seconds < 60 ? `${seconds} 초` : `${Math.floor(seconds / 60)} 분 ${seconds % 60} 초`;
}

export function endReasonLabel(reason: string): string {
  if (reason === "budget_exhausted") return "연구 예산 소진";
  if (reason === "blocked") return "외부 조건 대기";
  if (reason === "stalled") return "연속 검사에서 새 증거 없음";
  return ({ completed: "정상 종료", incomplete_stream: "응답 스트림 중단", service_restart: "서비스 재시작 중단", cancelled: "취소됨", timeout: "시간 상한 도달", max_turns: "단계 수 상한 도달", model_error: "모델 요청 실패", runner_error: "실행 이상", aborted_streaming: "스트리밍 응답 중지됨", aborted_tools: "도구 실행 중지됨", blocking_limit: "컨텍스트 상한 도달", prompt_too_long: "컨텍스트 과다", image_error: "이미지 처리 실패", stop_hook_prevented: "종료 검사 미통과", hook_stopped: "검사 규칙이 실행을 중지" } as Record<string, string>)[reason] ?? reason;
}

export function verificationLevelLabel(level: string): string {
  return ({ inference: "추론", observed: "실제 관찰", reproduced: "격리 실험 재현", accepted: "플랫폼 수락" } as Record<string, string>)[level] ?? "미분류";
}
