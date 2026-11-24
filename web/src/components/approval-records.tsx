"use client";

import * as React from "react";

import {
  BotIcon,
  CheckIcon,
  ChevronDownIcon,
  ChevronUpIcon,
  CopyIcon,
  XIcon
} from "lucide-react";
import { toast } from "sonner";

import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Skeleton } from "@/components/ui/skeleton";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { api } from "@/lib/api";
import type {
  InterceptApprovalRow,
  InterceptAudit,
  InterceptDetail,
  InterceptReviewInput
} from "@/lib/types";
import { cn } from "@/lib/utils";

function fmtTime(value?: string) {
  if (!value) return "—";
  return new Date(value).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function source(row: InterceptApprovalRow) {
  if (row.decision_source) return row.decision_source;
  if (row.rule_id) return "rule";
  return (row.reason?.startsWith("[模型]") || row.reason?.startsWith("[모델]")) ? "model" : "unknown";
}

function originLabel(row: InterceptApprovalRow) {
  if (row.task_id) return row.task_id;
  if (row.conversation_id) return `대화 #${row.conversation_id}`;
  return "—";
}

function ApprovalOrigin({ row, detail = false }: { row: InterceptApprovalRow; detail?: boolean }) {
  return <span>{detail && row.task_id ? `Task ${row.task_id}` : originLabel(row)}</span>;
}

function StatusBadge({ status }: { status: string }) {
  const labels: Record<string, string> = { pending: "승인 대기", allowed: "허용됨", denied: "거부됨", timeout: "시간 초과" };
  let variant: "default" | "destructive" | "secondary" | "outline" = "outline";
  if (status === "allowed") variant = "default";
  if (status === "denied") variant = "destructive";
  if (status === "pending") variant = "secondary";
  return <Badge variant={variant}>{labels[status] ?? status}</Badge>;
}

function MatchCell({ row, showReason = true }: { row: InterceptApprovalRow; showReason?: boolean }) {
  const reason = row.reason?.replace(/^\[(?:模型|모델)\]\s*/, "");
  const decisionSource = source(row);
  return (
    <div className="flex min-w-0 flex-col gap-1">
      {decisionSource === "model" ? (
        <Badge variant="outline">
          <BotIcon />
          모델 판정
        </Badge>
      ) : decisionSource === "manual" ? (
        <Badge variant="outline">수동 판정</Badge>
      ) : (
        <span className="truncate">{row.rule_name || "규칙이 기록되지 않았거나 삭제됨"}</span>
      )}
      {showReason ? (
        <p className="truncate text-muted-foreground text-xs" title={reason}>
          {reason || "이유가 기록되지 않음"}
        </p>
      ) : null}
    </div>
  );
}

function CodeBlock({ label, text, truncated = false }: { label: string; text: string; truncated?: boolean }) {
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      toast.success("복사했습니다");
    } catch {
      toast.error("복사에 실패했습니다. 내용을 직접 선택해 복사하세요");
    }
  }
  return (
    <section className="flex min-w-0 flex-col gap-2" aria-label={label}>
      <div className="flex items-center justify-between gap-2">
        <h3 className="font-medium text-muted-foreground text-xs">{label}</h3>
        {text ? (
          <Button variant="ghost" size="icon-xs" aria-label={`${label} 복사`} onClick={() => void copy()}>
            <CopyIcon />
          </Button>
        ) : null}
      </div>
      <pre className="max-h-80 min-w-0 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-muted/60 p-3 font-mono text-xs leading-6 [overflow-wrap:anywhere]">
        {text || "기록되지 않음"}
      </pre>
      {truncated ? <p className="text-muted-foreground text-xs">내용이 잘렸습니다. 위는 저장된 조각입니다.</p> : null}
    </section>
  );
}

const contextLabels: Record<string, string> = {
  user: "사용자 메시지",
  assistant: "Agent 메시지",
  text: "Agent 메시지",
  tool_use: "도구 요청",
  tool_result: "도구 출력",
};

const actionLabels: Record<string, string> = { allow: "허용", ask: "수동 승인으로 전환", deny: "거부" };
const executionLabels: Record<InterceptAudit["execution_status"], string> = {
  not_started: "아직 실행 안 함",
  not_executed: "실행 안 됨",
  awaiting_result: "허용됨, 실행 결과 대기",
  succeeded: "실행 성공",
  failed: "실행 실패",
  unknown: "실행 결과 알 수 없음",
};

function ReviewBackgroundBlock({ background }: { background?: InterceptReviewInput["background"] }) {
  if (!background) return <p className="text-muted-foreground text-xs">이번 검토에는 배경 메시지가 없습니다.</p>;
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <CodeBlock
        label={background.source === "user_message" ? "배경 · 사용자 메시지" : "배경 · Worker 의도 요약(구 버전)"}
        text={background.text}
        truncated={background.truncated}
      />
      <p className="text-muted-foreground text-xs">
        {background.source === "user_message"
          ? "현재 사용자 메시지에서 가져옴."
          : "이것은 구 버전이 보낸 Worker 의도 요약입니다. 새 버전 Worker 검토는 더 이상 이 내용을 보내지 않습니다."}
      </p>
    </div>
  );
}

function ScriptSnapshotSummary({ input }: { input: InterceptReviewInput }) {
  const files = input.script_files ?? [];
  if (!files.length) return null;
  return (
    <section className="flex min-w-0 flex-col gap-2 rounded-lg border bg-background p-3" aria-label="첨부된 스크립트 스냅샷">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-medium text-sm">검토 스크립트 스냅샷</h3>
        <Badge variant={input.script_capture_status === "incomplete" ? "destructive" : "secondary"}>
          {input.script_capture_status === "incomplete" ? "읽기 불완전" : "첨부됨"}
        </Badge>
      </div>
      {files.map((file) => (
        <ScriptFileSnapshot key={file.path} file={file} />
      ))}
      <p className="text-muted-foreground text-xs">이것은 실행 전에 실제로 검토 모델에 보낸 스크립트 스냅샷입니다.</p>
    </section>
  );
}

function ScriptFileSnapshot({ file }: { file: NonNullable<InterceptReviewInput["script_files"]>[number] }) {
  const [open, setOpen] = React.useState(false);
  return (
    <div className="flex min-w-0 flex-col gap-2 overflow-visible">
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <code className="break-all">{file.path}</code>
        {file.sha256 ? <span className="text-muted-foreground">SHA-256 {file.sha256.slice(0, 12)}…</span> : null}
        {file.bytes != null ? <span className="text-muted-foreground">{file.bytes} 바이트</span> : null}
      </div>
      {file.read_error ? <p className="text-destructive text-xs">{file.read_error}</p> : null}
      {file.content ? (
        <>
          <Button
            variant="ghost"
            size="sm"
            className="self-start px-0"
            aria-expanded={open}
            onClick={() => setOpen((value) => !value)}
          >
            {open ? <ChevronUpIcon data-icon="inline-start" /> : <ChevronDownIcon data-icon="inline-start" />}
            스크립트 내용 보기
          </Button>
          {open ? <CodeBlock label="스크립트 내용(검토 스냅샷)" text={file.content} /> : null}
        </>
      ) : null}
    </div>
  );
}

function ModelReviewContext({ input }: { input: InterceptReviewInput }) {
  return (
    <section className="flex min-w-0 flex-col gap-4" aria-label="모델 검토 컨텍스트">
      <div className="flex flex-col gap-2">
        <h3 className="font-medium text-sm">모델 검토 컨텍스트</h3>
        <p className="text-muted-foreground text-xs">
          아래는 이번에 실제로 검토 모델에 보낸 입력 스냅샷입니다. 배경은 현재 동작을 이해하는 데만 쓰며, 판정 근거는 검토 정책입니다.
          {input.version < 4 ? "이 기록은 구 버전 입력을 쓰며, 당시 실제로 보낸 내용을 유지합니다." : null}
        </p>
      </div>
      <CodeBlock
        label="현재 검토 대상 호출"
        text={JSON.stringify({ tool_name: input.tool_name, arguments: input.arguments }, null, 2)}
      />
      {input.version >= 2 ? (
        <ReviewBackgroundBlock background={input.background} />
      ) : (
        <>
          {input.turn_input ? (
            <CodeBlock label="현재 턴 입력(구 버전)" text={input.turn_input} truncated={input.background_truncated} />
          ) : null}
          {input.task ? (
            <>
              <CodeBlock label="작업 설명(구 버전)" text={input.task.description} truncated={input.task.truncated} />
              <CodeBlock label="작업 목표(구 버전)" text={input.task.goal} truncated={input.task.truncated} />
              <CodeBlock label="작업 작업 제약(구 버전)" text={JSON.stringify(input.task.constraints, null, 2)} />
            </>
          ) : null}
          {input.worker_intent ? (
            <CodeBlock label="Worker 의도(구 버전)" text={input.worker_intent} truncated={input.background_truncated} />
          ) : null}
        </>
      )}
      {input.working_directory ? <CodeBlock label="작업 디렉터리" text={input.working_directory} /> : null}
      {input.script_files?.length ? (
        <section className="flex min-w-0 flex-col gap-4" aria-label="스크립트 스냅샷">
          <div>
            <h3 className="font-medium text-sm">스크립트 스냅샷</h3>
            <p className="text-muted-foreground text-xs">
              이것은 검토 전에 현재 명령이나 참조 파일에서 추출한 스크립트 내용입니다. 검토 과정에서 스크립트를 실행하지 않습니다.
            </p>
          </div>
          {input.script_files.map((file) => (
            <div key={`${file.path}:${file.source ?? ""}:${file.sha256 ?? file.read_error ?? ""}`} className="flex min-w-0 flex-col gap-2 rounded-lg border p-3">
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
                <span className="break-all font-mono">{file.path}</span>
                {file.source ? <span className="text-muted-foreground">{file.source === "command" ? "명령 내 생성" : "참조 파일"}</span> : null}
                {file.sha256 ? <span className="text-muted-foreground">SHA-256 {file.sha256}</span> : null}
                {file.bytes != null ? <span className="text-muted-foreground">{file.bytes} 바이트</span> : null}
              </div>
              {file.content ? <CodeBlock label="스크립트 내용" text={file.content} /> : null}
              {file.read_error ? (
                <Alert variant="destructive">
                  <AlertDescription>{file.read_error}</AlertDescription>
                </Alert>
              ) : null}
              {file.truncated ? (
                <p className="text-muted-foreground text-xs">스크립트 내용이 불완전해 이를 근거로 자동 통과시킬 수 없습니다.</p>
              ) : null}
            </div>
          ))}
          {input.script_capture_status === "incomplete" ? (
            <Alert variant="destructive">
              <AlertDescription>현재 실행 스크립트를 완전히 읽을 수 없습니다. 기본적으로 모델 검토에 계속 넘기며, 모델 설정에서 수동 검토로 전환할 수도 있습니다.</AlertDescription>
            </Alert>
          ) : null}
        </section>
      ) : null}
      {input.version >= 3 ? (
        <p className="text-muted-foreground text-xs">이번 검토에는 과거 호출이나 실행 결과를 보내지 않았습니다.</p>
      ) : (
        <div className="flex min-w-0 flex-col gap-3">
          <h4 className="font-medium text-muted-foreground text-xs">당시 모델에 제공한 과거 호출(구 버전)</h4>
          {input.history?.length ? (
            input.history.map((entry) => (
              <div key={entry.tool_use_id} className="flex min-w-0 flex-col gap-2 rounded-lg border p-3">
                <p className="break-words font-medium text-xs">
                  {entry.tool} · {entry.status === "succeeded" ? "성공" : "실패(일부 부수 효과가 있을 수 있음)"}
                </p>
                <CodeBlock label="과거 호출 파라미터" text={entry.arguments_preview} truncated={entry.truncated} />
                <CodeBlock label="과거 실행 결과" text={entry.result} truncated={entry.truncated} />
              </div>
            ))
          ) : (
            <p className="text-muted-foreground text-xs">이번에는 짝지을 수 있는 과거 도구 실행 기록을 제공하지 않았습니다.</p>
          )}
          {input.history_truncated ? (
            <p className="text-muted-foreground text-xs">과거 기록은 제한된 창이며, 일부 내용이 잘렸습니다.</p>
          ) : null}
        </div>
      )}

      <Collapsible>
        <CollapsibleTrigger asChild>
          <Button variant="outline" size="sm" className="self-start">
            <ChevronDownIcon data-icon="inline-start" />
            전체 모델 검토 입력 JSON 보기
          </Button>
        </CollapsibleTrigger>
        <CollapsibleContent className="pt-3">
          <CodeBlock label="모델 검토 입력" text={JSON.stringify(input, null, 2)} />
        </CollapsibleContent>
      </Collapsible>
    </section>
  );
}

type Decide = (id: number, decision: "allowed" | "denied") => Promise<void>;

function DecisionActions({ row, busy, decide }: { row: InterceptApprovalRow; busy: boolean; decide: Decide }) {
  if (row.status !== "pending") return null;
  return (
    <div className="flex flex-wrap gap-2">
      <Button size="sm" disabled={busy} onClick={() => void decide(row.id, "allowed")}>
        <CheckIcon data-icon="inline-start" />
        허용
      </Button>
      <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(row.id, "denied")}>
        <XIcon data-icon="inline-start" />
        거부
      </Button>
    </div>
  );
}

export function ApprovalDetail({
  row,
  busy,
  decide,
  revision,
  readOnly = false,
  defaultExpanded = false,
  onResolved,
}: {
  row: InterceptApprovalRow;
  busy: boolean;
  decide: Decide;
  revision: number;
  readOnly?: boolean;
  defaultExpanded?: boolean;
  onResolved?: (status: "allowed" | "denied" | "timeout") => void;
}) {
  const [detail, setDetail] = React.useState<InterceptDetail | null>(null);
  const [error, setError] = React.useState("");
  const [retry, setRetry] = React.useState(0);
  const [more, setMore] = React.useState(defaultExpanded);
  const side = useSideQuestions(`/api/intercept/history/${row.id}`);

  // biome-ignore lint/correctness/useExhaustiveDependencies: Status, refresh and retry invalidate details without closing the panel.
  React.useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function load() {
      try {
        const next = await api.interceptDetail(row.id);
        if (cancelled) return;
        setDetail(next);
        setError("");
        if (next.status !== "pending") onResolved?.(next.status);
        if (next.status === "pending" || next.audit?.execution_status === "awaiting_result")
          timer = setTimeout(() => void load(), 5000);
      } catch (e) {
        if (!cancelled) setError((e as Error).message || "상세 로드 실패");
      }
    }
    void load();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [row.id, row.status, revision, retry, onResolved]);

  // Row updates are authoritative until the lazy detail has caught up.
  const current = detail?.status === row.status ? detail : row;
  const audit = detail?.audit;
  let execution = audit ? executionLabels[audit.execution_status] : "기록되지 않음";
  if (row.status === "pending") execution = "아직 실행 안 함";
  if (audit && audit.correlation !== "exact" && audit.effective_action === "allow") execution = "실행 결과와 연결되지 않음";
  const command = typeof row.tool_input?.command === "string" ? row.tool_input.command : undefined;
  let initialLabel = source(row) === "model" ? "모델 초기 판정" : source(row) === "manual" ? "수동 판정" : "규칙 초기 판정";
  if (audit?.model_fallback) initialLabel = "모델 이상 대비";

  return (
    <SideQuestionWorkspace
      side={side}
      label={`승인 #${row.id} · ${row.tool_name}`}
      composerLayout="stacked"
      placeholder="이 승인의 스크립트나 검토 이유를 질문…"
      suggestions={[
        "이 스크립트의 역할은 무엇인가요?",
        "스크립트가 어떤 파일이나 데이터를 수정하나요?",
        "이번에 왜 통과되거나 차단되었나요?",
        "스크립트에 확정되지 않은 동작이 있나요?",
      ]}
      className={cn(side.open ? "min-h-[460px]" : "min-h-0")}
      mainClassName="overflow-visible"
    >
      <div className="flex min-w-0 flex-col gap-4 p-3 sm:p-5">
        <div className="grid min-w-0 gap-5 rounded-xl border bg-muted/20 p-4 lg:grid-cols-2">
          <div className="flex min-w-0 flex-col gap-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="font-medium text-sm">승인 컨텍스트</span>
              <SideQuestionButton side={side} />
            </div>
            <CodeBlock
              label={`${current.agent_name || current.conv_agent_key || "Agent"} · 도구 요청`}
              text={JSON.stringify(row.tool_input ?? {}, null, 2)}
            />
            {command ? (
              <Collapsible>
                <CollapsibleTrigger asChild>
                  <Button variant="ghost" size="sm">
                    <ChevronDownIcon data-icon="inline-start" />
                    명령 내용 보기
                  </Button>
                </CollapsibleTrigger>
                <CollapsibleContent className="pt-2">
                  <CodeBlock label="명령 내용" text={command} />
                </CollapsibleContent>
              </Collapsible>
            ) : null}
            <p className="text-muted-foreground text-xs">
              실행 결과: <span className="text-foreground">{detail ? execution : "로딩 중…"}</span>
            </p>
            {audit?.model_input ? <ScriptSnapshotSummary input={audit.model_input} /> : null}
          </div>
          <div className="flex min-w-0 flex-col gap-4 lg:border-l lg:pl-5">
            <h3 className="font-medium text-muted-foreground text-xs">
              {current.status === "pending" ? "검토 상태" : "승인 판정"}
            </h3>
            <div className="flex flex-wrap items-center gap-2">
              <StatusBadge status={current.status} />
              <MatchCell row={current} showReason={false} />
            </div>
            <p className="whitespace-pre-wrap break-words text-sm leading-7 [overflow-wrap:anywhere]">
              {current.reason?.replace(/^\[(?:模型|모델)\]\s*/, "") || "승인 이유가 기록되지 않음"}
            </p>
            {audit?.decision_reason ? <p className="text-sm">{audit.decision_reason}</p> : null}
            {audit?.effective_action ? (
              <p className="text-sm">최종 동작: {actionLabels[audit.effective_action]}</p>
            ) : null}
            <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">
              <dt className="text-muted-foreground">출처</dt>
              <dd className="break-words">
                <ApprovalOrigin row={current} detail />
              </dd>
              <dt className="text-muted-foreground">신청 시각</dt>
              <dd>{fmtTime(row.created_at)}</dd>
              <dt className="text-muted-foreground">결정 시각</dt>
              <dd>{fmtTime(current.decided_at)}</dd>
              {audit?.rule_name ? (
                <>
                  <dt className="text-muted-foreground">규칙 스냅샷</dt>
                  <dd>{audit.rule_name}</dd>
                </>
              ) : null}
              {audit?.profile_id ? (
                <>
                  <dt className="text-muted-foreground">승인 모델 설정</dt>
                  <dd>#{audit.profile_id}</dd>
                </>
              ) : null}
            </dl>
            {!readOnly ? <DecisionActions row={current} busy={busy} decide={decide} /> : null}
          </div>
        </div>
        {error ? (
          <Alert variant="destructive">
            <AlertDescription>
              <div className="flex flex-wrap items-center gap-2">
                <span>상세 로드 실패: {error}</span>
                <Button variant="outline" size="sm" onClick={() => setRetry((v) => v + 1)}>
                  상세 다시 시도
                </Button>
              </div>
            </AlertDescription>
          </Alert>
        ) : null}
        {!detail && !error ? <Skeleton className="h-8 w-60" /> : null}
        {detail && !audit ? (
          <Alert>
            <AlertDescription>이 기록에는 승인 상세 스냅샷이 저장되지 않아 당시의 컨텍스트, 모델 초기 판정, 실행 출력을 복원할 수 없습니다.</AlertDescription>
          </Alert>
        ) : null}
        {audit ? (
          <Collapsible open={more} onOpenChange={setMore}>
            <CollapsibleTrigger asChild>
              <Button variant="ghost" size="sm">
                <ChevronDownIcon data-icon="inline-start" className={cn(more && "rotate-180")} />
                {more ? "컨텍스트 접기" : "컨텍스트와 실행 결과 보기"}
              </Button>
            </CollapsibleTrigger>
            <CollapsibleContent className="pt-4">
              <div className="flex min-w-0 flex-col gap-5">
                {audit.model_input ? (
                  <ModelReviewContext input={audit.model_input} />
                ) : (
                  <Alert>
                    <AlertDescription>
                      {audit.model_input_digest
                        ? "이 과거 기록은 검토 입력 지문만 저장하고 입력 원문은 저장하지 않아 당시 모델에 보낸 컨텍스트를 복원할 수 없습니다. 이는 검토 시 컨텍스트가 없었다는 뜻이 아닙니다. 새로 생기는 모델 판정은 입력 스냅샷을 유지합니다."
                        : "이 기록에는 모델 검토 입력이 저장되지 않았습니다. 규칙이 바로 판정했거나, 모델 호출 전에 이상이 발생했거나, 이전 버전에서 생긴 것일 수 있습니다."}
                    </AlertDescription>
                  </Alert>
                )}
                {audit.user_message || audit.context?.length ? (
                  <Collapsible>
                    <CollapsibleTrigger asChild>
                      <Button variant="ghost" size="sm">
                        <ChevronDownIcon data-icon="inline-start" />
                        {audit.model_input && audit.model_input.version >= 3
                          ? "대화 감사 조각 보기(모델에 보내지 않음)"
                          : "대화 감사 조각 보기"}
                      </Button>
                    </CollapsibleTrigger>
                    <CollapsibleContent className="pt-3">
                      <div className="flex min-w-0 flex-col gap-3">
                        {audit.user_message ? (
                          <CodeBlock
                            label="대화 현재 턴 입력(감사 조각)"
                            text={audit.user_message}
                            truncated={audit.user_truncated}
                          />
                        ) : null}
                        <section className="flex min-w-0 flex-col gap-3">
                          <h3 className="font-medium text-muted-foreground text-xs">볼 수 있는 대화 컨텍스트</h3>
                          <p className="text-muted-foreground text-xs">
                            {fmtTime(audit.captured_at)} 에 저장된 대화 기록 조각입니다. 모델이 실제로 사용한 내용은 “모델 검토 입력”을 기준으로 합니다.
                          </p>
                          {audit.context_truncated ? (
                            <p className="text-muted-foreground text-xs">최근 컨텍스트만 저장되며, 일부 내용이 잘렸습니다.</p>
                          ) : null}
                          {audit.context?.length ? (
                            audit.context.map((entry, index) => (
                              <CodeBlock
                                key={`${entry.kind}-${entry.tool_use_id || index}`}
                                label={`${contextLabels[entry.kind] ?? entry.kind}${entry.tool ? ` · ${entry.tool}` : ""}${entry.is_error ? " · 이상" : ""}`}
                                text={entry.text}
                                truncated={entry.truncated}
                              />
                            ))
                          ) : (
                            <p className="text-muted-foreground text-sm">연결할 수 있는 컨텍스트가 기록되지 않음</p>
                          )}
                        </section>
                      </div>
                    </CollapsibleContent>
                  </Collapsible>
                ) : null}
                <CodeBlock
                  label={`${initialLabel}：${actionLabels[audit.initial_action] ?? audit.initial_action}`}
                  text={audit.initial_reason.replace(/^\[(?:模型|모델)\]\s*/, "")}
                />
                <CodeBlock
                  label="실행 출력"
                  text={audit.output ?? (audit.execution_status === "not_executed" ? "도구가 실행되지 않았습니다." : "아직 실행 출력이 없습니다")}
                  truncated={audit.output_truncated}
                />
                {audit.correlation !== "exact" ? (
                  <Alert>
                    <AlertDescription>
                      {audit.correlation === "ambiguous"
                        ? "같은 파라미터의 동시 호출이 있어 도구 호출을 고유하게 연결할 수 없습니다. 이 기록은 추측한 실행 결과를 보여 주지 않습니다."
                        : "고유하게 연결할 수 있는 도구 호출 ID 가 기록되지 않았습니다."}
                    </AlertDescription>
                  </Alert>
                ) : null}
                <dl className="grid gap-2 text-muted-foreground text-xs [overflow-wrap:anywhere]">
                  <div>도구 호출 ID: {audit.tool_use_id || "기록되지 않음"}</div>
                  <div>파라미터 요약 SHA-256: {audit.input_digest}</div>
                  <div>검토 설정 지문 SHA-256: {audit.config_digest || "기록되지 않음"}</div>
                  {audit.model_input_digest ? <div>모델 검토 입력 SHA-256: {audit.model_input_digest}</div> : null}
                  {audit.execution_ended_at ? <div>결과 기록 시각: {fmtTime(audit.execution_ended_at)}</div> : null}
                </dl>
              </div>
            </CollapsibleContent>
          </Collapsible>
        ) : null}
      </div>
    </SideQuestionWorkspace>
  );
}
