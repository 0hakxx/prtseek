"use client";

import * as React from "react";

import { toast } from "sonner";
import { NativeSelect } from "@/components/ui/native-select";

import { personalApi } from "@/lib/personal-api";

const labels: Record<string, string> = {
  none: "끔",
  minimal: "최소",
  low: "낮음",
  medium: "중간",
  high: "높음",
  xhigh: "아주 높음",
  max: "최대",
  ultra: "극히 높음",
};

export function ReasoningControl({
  profileId,
  value,
  onChange,
  disabled = false,
}: {
  profileId?: number;
  value: string;
  onChange: (value: string) => void | Promise<void>;
  disabled?: boolean;
}) {
  const [efforts, setEfforts] = React.useState<string[]>([]);
  const [busy, setBusy] = React.useState(false);
  const [loading, setLoading] = React.useState(false);
  const [hint, setHint] = React.useState("");
  const version = React.useRef(0);
  React.useEffect(() => {
    const current = ++version.current;
    setEfforts([]);
    setBusy(false);
    setHint("");
    if (!profileId) {
      setLoading(false);
      return;
    }
    setLoading(true);
    personalApi
      .capabilities(profileId)
      .then((result) => {
        if (version.current !== current) return;
        setEfforts(result.reasoning_efforts.filter(Boolean));
        setHint(
          result.reasoning_efforts.filter(Boolean).length
            ? "기본이 아닌 강도를 선택하면 인터페이스 지원을 검증하며 다음 턴부터 적용됩니다"
            : "이 모델은 아직 조정 가능한 강도를 확인하지 않아 모델 기본값을 사용",
        );
      })
      .catch(() => {
        if (version.current === current) setHint("모델 능력을 읽을 수 없어 잠시 모델 기본값 사용");
      })
      .finally(() => {
        if (version.current === current) setLoading(false);
      });
    return () => {
      version.current++;
    };
  }, [profileId]);

  async function choose(next: string) {
    if (!profileId || busy) return;
    const current = version.current;
    setBusy(true);
    try {
      if (next) {
        const result = await personalApi.verifyEffort(profileId, next);
        if (!result.verified) throw new Error(result.error || "인터페이스가 이 강도를 확인하지 않았습니다. 모델 기본값을 사용하세요");
      }
      if (version.current === current) await onChange(next);
    } catch (e) {
      if (version.current === current) toast.error((e as Error).message);
    } finally {
      if (version.current === current) setBusy(false);
    }
  }

  return (
    <label
      className="flex min-h-11 items-center gap-2 border px-2 text-sm text-muted-foreground"
      title={hint}
    >
      <span>{busy ? "검증 중…" : "사고"}</span>
      <NativeSelect
        aria-label="사고 강도"
        value={value}
        onChange={(e) => void choose(e.target.value)}
        disabled={disabled === true || busy || loading || !profileId}
        className="max-w-36 border-0 bg-transparent py-1 pr-8 text-sm text-foreground focus-visible:ring-0"
      >
        <option value="">모델 기본값</option>
        {value && !efforts.includes(value) && <option value={value}>{labels[value] ?? value}(확인 대기)</option>}
        {efforts.map((effort) => (
          <option key={effort} value={effort}>
            {labels[effort] ?? effort}
          </option>
        ))}
      </NativeSelect>
    </label>
  );
}
