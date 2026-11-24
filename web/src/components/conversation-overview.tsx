"use client";

import * as React from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { api } from "@/lib/api";
import { elapsedLabel, executionLabel, outcomeLabel, endReasonLabel, verificationLevelLabel } from "@/lib/ctf-status";
import { hideResearchEvidenceAnnotation } from "@/lib/research-evidence";
import { type ConversationFile, type ConversationOverview as Overview, workflowsApi } from "@/lib/workflows-api";

export function ConversationOverview({ id, overview }: { id: number; overview: Overview | null }) {
  const [open, setOpen] = React.useState(false);
  const [files, setFiles] = React.useState<ConversationFile[]>([]);
  const [fileState, setFileState] = React.useState("");
  const [preview, setPreview] = React.useState<{ name: string; text: string } | null>(null);
  const [truncated, setTruncated] = React.useState(false);
  const previewRequest = React.useRef(0);

  async function loadFiles() {
    setFileState("대화 파일을 읽는 중…");
    try {
      const result = await workflowsApi.files(id);
      setFiles(result.items);
      setTruncated(result.truncated);
      setFileState("");
    } catch (e) { setFileState((e as Error).message); }
  }
  async function showFile(file: ConversationFile) {
    const version = ++previewRequest.current;
    setPreview({ name: file.name, text: "읽는 중…" });
    try {
      const result = await workflowsApi.preview(id, file);
      if (version === previewRequest.current) setPreview({ name: file.name, text: result.content ?? (result.too_large ? "파일이 커서 다운로드해서 보세요." : "바이너리 파일이라 다운로드해서 보세요.") });
    } catch (e) { if (version === previewRequest.current) setPreview({ name: file.name, text: (e as Error).message }); }
  }

  return <div className="shrink-0 border-b bg-background px-4 py-2 text-xs text-muted-foreground">
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
      {overview && <>
        <span>실행: {executionLabel(overview.state)}</span>
        <span className={overview.outcome.state === "blocked" ? "text-amber-600 dark:text-amber-400" : "text-foreground"}>목표: {outcomeLabel(overview.outcome.state)}</span>
        <span>이번 턴 {elapsedLabel(overview.elapsed_ms)}</span>
        <span title="모든 하위 대화 포함. 인터페이스가 보고한 사용량 누적">누적 Token {(overview.input_tokens + overview.output_tokens).toLocaleString()}</span>
        {!!overview.cache_read_tokens && <span title="인터페이스가 보고한 캐시 읽기량, 별도 표시">캐시 읽기 {overview.cache_read_tokens.toLocaleString()}</span>}
        {overview.end_reason && <span title={`실행 번호 ${overview.turn_id ?? "—"}`}>종료 원인: {endReasonLabel(overview.end_reason)}</span>}
        <span title="이번 턴과 하위 작업이 공유하는 논리적 모델 요청 예산. 하위 계층 네트워크 재시도는 별도">호출 {overview.model_calls}/{overview.model_call_limit}</span>
        {overview.active_children > 0 && <span>하위 작업 {overview.active_children} 개 실행 중</span>}
      </>}
      <Button size="sm" variant="outline" className="ml-auto" onClick={() => { setOpen(true); void loadFiles(); }}>대화 파일</Button>
    </div>
    {overview?.latest && <p className="mt-1 truncate" title={hideResearchEvidenceAnnotation(overview.latest.summary)}>최근 진행 상황: {overview.latest.worker} · {overview.latest.tool || overview.latest.kind} · {hideResearchEvidenceAnnotation(overview.latest.summary)}</p>}
    {overview?.outcome.summary && <details className="mt-1">
      <summary className="cursor-pointer text-foreground">목표 보고: {overview.outcome.summary}</summary>
      <div className="space-y-1 py-2 whitespace-pre-wrap break-words">
        <p>검수 조건: {overview.outcome.criterion}</p>
        {overview.outcome.verification_level && <p>검증 등급: {verificationLevelLabel(overview.outcome.verification_level)} · Agent 보고</p>}
        {overview.outcome.verification && <p>검증 기록: {overview.outcome.verification}</p>}
        {!!overview.outcome.evidence?.length && <p>증거: {overview.outcome.evidence.map((e) => `#${e.conversation_id}/${e.id}`).join(", ")}</p>}
      </div>
    </details>}
    <Dialog open={open} onOpenChange={(value) => { setOpen(value); if (!value) { previewRequest.current++; setPreview(null); } }}>
      <DialogContent className="flex max-h-[85dvh] flex-col sm:max-w-3xl">
        <DialogHeader><DialogTitle>이 문제의 파일과 산출물</DialogTitle><DialogDescription>현재 대화와 하위 작업의 첨부 파일, 스크립트, WriteUP, 증거를 포함합니다.</DialogDescription></DialogHeader>
        <div className="flex items-center justify-between gap-2">
          <a className="text-sm underline" href={`/function/workspace?path=${encodeURIComponent(`sessions/conv-${id}`)}`}>대화 작업 공간 열기</a>
          <Button size="sm" variant="outline" onClick={() => void loadFiles()}>파일 새로 고침</Button>
        </div>
        {fileState && <p role="status" className="text-sm">{fileState}</p>}
        <div className="min-h-0 overflow-y-auto">
          {!fileState && files.length === 0 && <p className="py-8 text-center text-sm text-muted-foreground">이 대화에는 아직 파일이 없습니다.</p>}
          <ul className="divide-y">
            {files.map((file) => <li key={`${file.conversation_id}/${file.path}`} className="flex flex-wrap items-center gap-2 py-2 text-sm">
              <button className="min-w-0 flex-1 text-left hover:underline" onClick={() => void showFile(file)}>
                <span className="block break-all font-medium">{file.name}</span>
                <span className="block break-all text-xs text-muted-foreground">{({ input: "첨부", artifact: "산출물", evidence: "증거" })[file.kind]} · #{file.conversation_id} · {file.path} · {file.size.toLocaleString()} B</span>
              </button>
              <Button size="sm" variant="ghost" onClick={() => void api.workspaceDownload(file.workspace_path).catch((e) => toast.error(e.message))}>다운로드</Button>
            </li>)}
          </ul>
          {truncated && <p className="py-2 text-sm text-muted-foreground">파일이 많아 일부 항목만 표시합니다. 작업 공간에서 전체를 볼 수 있습니다.</p>}
          {preview && <section className="mt-3 border-t pt-3"><strong className="break-all text-sm">{preview.name}</strong><pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-3 text-xs">{preview.text}</pre></section>}
        </div>
      </DialogContent>
    </Dialog>
  </div>;
}
