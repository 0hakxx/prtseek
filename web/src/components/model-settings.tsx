"use client";

import * as React from "react";

import { Check, ChevronDown, Plus, Search, Settings2, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { api } from "@/lib/api";
import type { LLMProfile } from "@/lib/types";

export function ModelEditor({
  profile,
  onSaved,
  initial = false,
}: {
  profile?: LLMProfile;
  onSaved: () => void;
  initial?: boolean;
}) {
  const [name, setName] = React.useState(profile?.name ?? "");
  const [format, setFormat] = React.useState<LLMProfile["format"]>(profile?.format ?? "openai");
  const [baseUrl, setBaseUrl] = React.useState(profile?.base_url ?? "");
  const [key, setKey] = React.useState("");
  const [model, setModel] = React.useState(profile?.model ?? "");
  const [models, setModels] = React.useState<string[]>([]);
  const [modelQuery, setModelQuery] = React.useState("");
  const [modelsOpen, setModelsOpen] = React.useState(false);
  const [makeDefault, setMakeDefault] = React.useState(initial === true || profile?.is_default === true);
  const [busy, setBusy] = React.useState("");
  const [error, setError] = React.useState("");
  const [tested, setTested] = React.useState("");
  const [advanced, setAdvanced] = React.useState({
    proxy: profile?.proxy ?? "",
    session_header_key: profile?.session_header_key ?? "",
    reasoning_effort: profile?.reasoning_effort ?? "",
    thinking_type: profile?.thinking_type ?? "",
    streaming: profile?.streaming ?? true,
    context_window_k: profile?.context_window_k ?? 0,
    max_tokens: profile?.max_tokens ?? 0,
    max_tokens_field: profile?.max_tokens_field ?? "",
  });
  const id = profile ? Number(profile.id) : undefined;

  async function loadModels() {
    setBusy("models");
    setError("");
    try {
      const result = await api.fetchLLMModels(format, baseUrl.trim(), key, advanced.proxy, id);
      if (!result.ok || !result.models?.length)
        throw new Error(result.error || "인터페이스가 모델 목록을 반환하지 않았습니다. 모델 ID 를 직접 입력하세요.");
      const complete = [...new Set(result.models)].sort((a, b) => a.localeCompare(b));
      setModels(complete);
      if (!model) setModel(result.models[0]);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }
  async function save() {
    setError("");
    setTested("");
    if (!model.trim() || (!key.trim() && !profile?.api_key_hint)) {
      setError("API Key 와 모델을 입력하세요.");
      return;
    }
    setBusy("save");
    try {
      const result = await api.testLLM(format, model.trim(), baseUrl.trim(), key, advanced.proxy, advanced.thinking_type, advanced.reasoning_effort, id, advanced.streaming, advanced.session_header_key, true, { max_tokens: advanced.max_tokens, max_tokens_field: advanced.max_tokens_field });
      if (!result.ok) throw new Error(result.error || "모델이 유효한 응답을 반환하지 않았습니다");
      if (!result.tools_verified) throw new Error("도구 호출 능력이 아직 검증되지 않았습니다.");
      setTested(`연결과 도구 왕복이 검증됨 · ${result.latency_ms ?? "—"} ms`);
      const saved = await api.saveLLMProfile({
        ...(profile ? { ...profile, id } : {}),
        name: name.trim() || model.trim(),
        format,
        base_url: baseUrl.trim(),
        api_key: key,
        model: model.trim(),
        ...advanced,
        pool_exclude: profile?.pool_exclude ?? true,
      });
      if (makeDefault || initial) await api.activateLLMProfile(String(saved.id));
      toast.success("모델 설정을 검증하고 저장했습니다");
      window.dispatchEvent(new Event("prtseek-models-updated"));
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  }

  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <Label htmlFor="model-format">인터페이스 유형</Label>
        <NativeSelect
          id="model-format"
          className="w-full"
          value={format}
          onChange={(e) => setFormat(e.target.value as LLMProfile["format"])}
          disabled={!!busy}
        >
          <option value="openai">OpenAI 호환 인터페이스</option>
          <option value="openai-responses">Responses 인터페이스</option>
          <option value="anthropic">Anthropic 인터페이스</option>
        </NativeSelect>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="model-url">Base URL</Label>
        <Input
          id="model-url"
          placeholder={format === "anthropic" ? "https://api.anthropic.com" : "https://api.example.com/v1"}
          value={baseUrl}
          onChange={(e) => setBaseUrl(e.target.value)}
          disabled={!!busy}
          autoComplete="off"
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="model-key">API Key / SK</Label>
        <Input
          id="model-key"
          type="password"
          placeholder={profile?.api_key_hint ? `${profile.api_key_hint} 저장됨, 비우면 유지` : "서비스 제공자가 준 키를 입력"}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          disabled={!!busy}
          autoComplete="new-password"
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="model-id">모델</Label>
        <div className="flex flex-col gap-2 sm:flex-row">
          <Input
            id="model-id"
            placeholder="모델 ID 를 선택하거나 입력"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            disabled={!!busy}
          />
          <Button
            type="button"
            variant="outline"
            onClick={loadModels}
            disabled={!!busy || (!key && !profile?.api_key_hint)}
          >
            {busy === "models" ? "가져오는 중…" : "모델 가져오기"}
          </Button>
        </div>
        {models.length > 0 && (
          <Popover open={modelsOpen} onOpenChange={setModelsOpen}>
            <PopoverTrigger asChild>
              <Button type="button" variant="outline" className="w-full justify-between font-normal">
                <span>모든 모델 보기</span>
                <span className="flex items-center gap-2 text-muted-foreground">
                  {models.length} 개
                  <ChevronDown className="size-4" />
                </span>
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-[var(--radix-popover-trigger-width)] gap-0 border p-0">
              <div className="flex items-center gap-2 border-b px-3">
                <Search className="size-4 shrink-0 text-muted-foreground" />
                <Input
                  aria-label="모델 검색"
                  value={modelQuery}
                  onChange={(event) => setModelQuery(event.target.value)}
                  placeholder="모델 ID 로 검색"
                  className="border-0 bg-transparent px-0 focus-visible:ring-0"
                />
              </div>
              <div role="listbox" aria-label="전체 모델 목록" className="max-h-80 overflow-y-auto p-1">
                {models
                  .filter((item) => item.toLowerCase().includes(modelQuery.trim().toLowerCase()))
                  .map((item) => (
                    <button
                      key={item}
                      type="button"
                      role="option"
                      aria-selected={model === item}
                      className="flex min-h-11 w-full items-center justify-between border-b px-3 py-2 text-left font-mono text-sm last:border-b-0 hover:bg-accent focus-visible:bg-accent"
                      onClick={() => {
                        setModel(item);
                        setModelsOpen(false);
                      }}
                    >
                      <span className="min-w-0 break-all">{item}</span>
                      {model === item && <Check className="ml-3 size-4 shrink-0 text-primary" />}
                    </button>
                  ))}
                {models.every((item) => !item.toLowerCase().includes(modelQuery.trim().toLowerCase())) && (
                  <p className="p-4 text-center text-sm text-muted-foreground">일치하는 모델이 없습니다</p>
                )}
              </div>
            </PopoverContent>
          </Popover>
        )}
        <p className="text-xs text-muted-foreground">
          {models.length > 0
            ? `전체 ${models.length} 개 모델을 읽었습니다. 검색, 선택하거나 모델 ID 를 직접 입력할 수 있습니다.`
            : "서비스가 모델 목록을 지원하지 않으면 모델 ID 를 직접 입력할 수 있습니다."}
        </p>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="model-name">설정 이름(선택)</Label>
        <Input
          id="model-name"
          placeholder="기본적으로 모델 이름 사용"
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={!!busy}
        />
      </div>
      {!initial && (
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={makeDefault}
            onChange={(e) => setMakeDefault(e.target.checked)}
            disabled={!!busy}
          />
          기본 모델로 설정
        </label>
      )}
      <details className="rounded-md border p-3">
        <summary className="cursor-pointer text-sm">고급 연결과 모델 파라미터</summary>
        <div className="mt-3 space-y-3">
          <p className="text-xs text-muted-foreground">수정하지 않은 파라미터는 유지됩니다. 저장 전에 실제 도구 호출과 결과 전달을 검증합니다.</p>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={advanced.streaming} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, streaming: e.target.checked })} />스트리밍 응답 사용</label>
          <div className="space-y-1"><Label htmlFor="model-proxy">연결 프록시</Label><Input id="model-proxy" value={advanced.proxy} disabled={!!busy} placeholder="http://127.0.0.1:7890" onChange={(e) => setAdvanced({ ...advanced, proxy: e.target.value })} /></div>
          <div className="space-y-1"><Label htmlFor="model-session-header">사용자 정의 대화 헤더 이름</Label><Input id="model-session-header" value={advanced.session_header_key} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, session_header_key: e.target.value })} /></div>
          <div className="space-y-1"><Label htmlFor="model-effort">기본 사고 강도</Label><Input id="model-effort" value={advanced.reasoning_effort} disabled={!!busy} placeholder="비우면 인터페이스 기본값 사용" onChange={(e) => setAdvanced({ ...advanced, reasoning_effort: e.target.value })} /></div>
          <div className="space-y-1"><Label htmlFor="model-thinking">사고 모드</Label><NativeSelect id="model-thinking" value={advanced.thinking_type} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, thinking_type: e.target.value })}><option value="">인터페이스 기본</option><option value="enabled">켬</option><option value="disabled">끔</option></NativeSelect></div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1"><Label htmlFor="model-context">컨텍스트 용량(K Token)</Label><Input id="model-context" type="number" min={0} value={advanced.context_window_k} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, context_window_k: Math.max(0, Number(e.target.value)) })} /></div>
            <div className="space-y-1"><Label htmlFor="model-output">출력 상한(Token)</Label><Input id="model-output" type="number" min={0} value={advanced.max_tokens} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, max_tokens: Math.max(0, Number(e.target.value)) })} /></div>
          </div>
          <p className="text-xs text-muted-foreground">용량 0 은 앱 기본값을, 출력 상한 0 은 인터페이스 기본값을 사용합니다.</p>
          <div className="space-y-1"><Label htmlFor="model-output-field">출력 상한 필드</Label><NativeSelect id="model-output-field" value={advanced.max_tokens_field} disabled={!!busy} onChange={(e) => setAdvanced({ ...advanced, max_tokens_field: e.target.value })}><option value="">max_tokens(기본)</option><option value="max_completion_tokens">max_completion_tokens</option></NativeSelect></div>
        </div>
      </details>
      {error && (
        <p role="alert" className="rounded-md bg-destructive/10 p-3 text-sm text-destructive whitespace-pre-wrap">
          {error}
        </p>
      )}
      {tested && (
        <p role="status" className="text-sm text-emerald-600">
          {tested}
        </p>
      )}
      <Button className="w-full" onClick={save} disabled={!!busy}>
        {busy === "save" ? "테스트하고 저장하는 중…" : "테스트하고 저장"}
      </Button>
    </div>
  );
}

export function ModelSettings() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [editor, setEditor] = React.useState<LLMProfile | "new" | null>(null);
  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
      setError("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);
  async function activate(p: LLMProfile) {
    try {
      await api.activateLLMProfile(String(p.id));
      await load();
      window.dispatchEvent(new Event("prtseek-models-updated"));
    } catch (e) {
      toast.error((e as Error).message);
    }
  }
  async function remove(p: LLMProfile) {
    if (!window.confirm(`모델 설정 “${p.name}” 을 삭제하시겠습니까? 과거 대화는 삭제되지 않습니다.`)) return;
    try {
      await api.deleteLLMProfile(String(p.id));
      await load();
      window.dispatchEvent(new Event("prtseek-models-updated"));
    } catch (e) {
      toast.error((e as Error).message);
    }
  }
  return (
    <section className="space-y-5">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">모델 설정</h1>
          <p className="mt-1 text-sm text-muted-foreground">연결 정보를 저장하고, 각 대화 아래에서 모델과 사고 강도를 선택하세요.</p>
        </div>
        <Button onClick={() => setEditor("new")}>
          <Plus className="size-4" />
          모델 추가
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      )}
      {loading ? (
        <p className="text-sm text-muted-foreground">모델을 읽는 중…</p>
      ) : profiles.length === 0 ? (
        <div className="rounded-xl border border-dashed p-8 text-center">
          <p className="mb-4 text-muted-foreground">아직 모델 설정이 없습니다. 추가하면 대화를 시작할 수 있습니다.</p>
          <Button onClick={() => setEditor("new")}>첫 모델 설정하기</Button>
        </div>
      ) : (
        <div className="space-y-3">
          {profiles.map((p) => (
            <article key={p.id} className="flex flex-wrap items-center justify-between gap-4 rounded-xl border p-4">
              <div className="min-w-0">
                <div className="flex items-center gap-2 font-medium">
                  {p.name}
                  {p.is_default && <span className="rounded bg-primary/10 px-2 py-0.5 text-xs text-primary">기본</span>}
                </div>
                <p className="mt-1 break-all text-sm text-muted-foreground">
                  {p.model} · {p.format}
                </p>
              </div>
              <div className="flex items-center gap-1">
                {!p.is_default && (
                  <Button variant="ghost" size="sm" onClick={() => activate(p)}>
                    <Check className="size-4" />
                    기본으로 설정
                  </Button>
                )}
                <Button variant="ghost" size="sm" onClick={() => setEditor(p)}>
                  <Settings2 className="size-4" />
                  편집
                </Button>
                <Button variant="ghost" size="icon" aria-label={`${p.name} 삭제`} onClick={() => remove(p)}>
                  <Trash2 className="size-4" />
                </Button>
              </div>
            </article>
          ))}
        </div>
      )}
      <Dialog
        open={editor !== null}
        onOpenChange={(open) => {
          if (!open) setEditor(null);
        }}
      >
        <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{editor === "new" ? "모델 추가" : "모델 편집"}</DialogTitle>
          </DialogHeader>
          {editor && (
            <ModelEditor
              key={typeof editor === "string" ? editor : editor.id}
              profile={editor === "new" ? undefined : editor}
              initial={profiles.length === 0}
              onSaved={() => {
                setEditor(null);
                void load();
              }}
            />
          )}
        </DialogContent>
      </Dialog>
    </section>
  );
}
