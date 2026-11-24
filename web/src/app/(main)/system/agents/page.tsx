"use client";

import * as React from "react";

import { toast } from "sonner";

import { AgentEditor } from "@/components/agent-editor";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { api } from "@/lib/api";
import type { Agent } from "@/lib/types";

const DEFAULT_ROLES = [
  { key: "triage", phase: "01 / INTAKE", name: "CTF Triage", description: "문제, 첨부 파일, 환경, 범위, flag 형식을 점검한다." },
  { key: "command", phase: "02 / COMMAND", name: "CTF Command", description: "목표를 쪼개고 병렬 Solver 를 조직하며 실제 증거를 수렴한다." },
  { key: "solver", phase: "03 / EXECUTE", name: "CTF Solver", description: "문제 자료를 분석하고 재현 가능한 핵심 증거를 낸다." },
  { key: "writeup", phase: "04 / ARCHIVE", name: "CTF WriteUP", description: "검증된 결과, EXP, 재현 가능한 WRITEUP 을 정리한다." },
] as const;

export default function AgentsPage() {
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [editing, setEditing] = React.useState<string | null>(null);
  const [query, setQuery] = React.useState("");
  const reload = React.useCallback(() => {
    void api.agents().then(setAgents).catch((e) => toast.error(e.message));
  }, []);
  React.useEffect(reload, [reload]);

  const visible = DEFAULT_ROLES
    .map((role) => ({ role, agent: agents.find((agent) => agent.key === role.key) }))
    .filter(({ role, agent }) => `${role.key} ${role.name} ${role.description} ${agent?.description ?? ""}`.toLowerCase().includes(query.trim().toLowerCase()));

  return (
    <div className="mx-auto w-full max-w-5xl space-y-6">
      <div>
        <div className="prts-section-code">CTF / DEFAULT AGENTS</div>
        <h1 className="text-xl font-semibold">Agent 편성</h1>
      </div>
      <Input aria-label="Agent 검색" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="key, 이름 또는 설명 검색" />
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {visible.map(({ role, agent }) => (
          <section key={role.key} className="space-y-3 border p-5">
            <div className="font-mono text-xs text-primary">{role.phase}</div>
            <h2 className="font-mono">{role.key}</h2>
            <p className="text-sm font-medium">{agent?.name || role.name}</p>
            <p className="min-h-10 text-sm text-muted-foreground">{agent?.description || role.description}</p>
            {!agent && <p className="text-xs text-destructive">역할이 아직 초기화되지 않았습니다. 서비스를 재시작하면 자동으로 만들어집니다.</p>}
            {agent && !agent.enabled && <p className="text-xs text-muted-foreground">비활성화됨</p>}
            <Button size="sm" variant="outline" disabled={!agent} onClick={() => setEditing(role.key)}>Agent 설정</Button>
          </section>
        ))}
      </div>
      {visible.length === 0 && <p className="text-sm text-muted-foreground">일치하는 기본 Agent 가 없습니다.</p>}
      <Sheet open={!!editing} onOpenChange={(open) => !open && setEditing(null)}>
        <SheetContent side="right" className="w-full overflow-y-auto sm:max-w-3xl">
          <SheetHeader><SheetTitle>{editing}</SheetTitle><SheetDescription>기본 역할의 프롬프트, 모델, 실행 설정, 보이는 리소스를 편집합니다.</SheetDescription></SheetHeader>
          {editing && <AgentEditor agentKey={editing} onSaved={reload} />}
        </SheetContent>
      </Sheet>
    </div>
  );
}
