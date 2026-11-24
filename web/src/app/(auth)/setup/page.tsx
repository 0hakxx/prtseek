"use client";

import { useEffect, useState } from "react";

import { useRouter } from "next/navigation";

import { PrtsAuthShell } from "@/components/prts-auth-shell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function SetupPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  // 초기화 상태를 조회할 수 없을 때 기본적으로 "초기화 안 됨"으로 간주하면 안 됨——그러면 사실은 이미
  // 비밀번호를 설정한 인스턴스에 초기화 폼을 보여 주게 되고, 사용자가 그대로 입력하면 원래 비밀번호를 덮어쓴다. 이때는 진입을 닫는다.
  const [unavailable, setUnavailable] = useState(false);

  useEffect(() => {
    api
      .authStatus()
      .then(({ initialized }) => {
        if (initialized) router.replace("/login");
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : "백엔드 서비스에 연결할 수 없습니다");
        setUnavailable(true);
      })
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setError("두 번 입력한 비밀번호가 일치하지 않습니다");
      return;
    }
    if (password.length < 8) {
      setError("비밀번호는 최소 8자여야 합니다");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.initPassword(password);
      auth.setToken(token);
      router.replace("/chat");
    } catch (err) {
      setError(err instanceof Error ? err.message : "초기화 실패");
    } finally {
      setLoading(false);
    }
  }

  if (checking) return null;

  return (
    <PrtsAuthShell mode="INITIALIZE">
      <div className="space-y-3">
        <h2 className="text-2xl font-medium tracking-tight">{unavailable ? "초기화 상태를 확인할 수 없음" : "비밀번호 초기화"}</h2>
        <p className="text-sm leading-relaxed text-muted-foreground">
          {unavailable
            ? "백엔드나 데이터베이스를 일시적으로 사용할 수 없습니다. 인스턴스에 이미 있는 비밀번호를 덮어쓰지 않도록 초기화 진입을 임시로 닫았습니다——서비스를 복구한 뒤 다시 시도하세요."
            : "prtseek 를 처음 사용합니다. 계정에 로그인 비밀번호를 설정하세요(최소 8자)"}
        </p>
      </div>
      {unavailable ? (
        <div className="flex flex-col gap-4">
          {error && <p className="text-center text-sm text-destructive">{error}</p>}
          <Button type="button" className="w-full" onClick={() => window.location.reload()}>
            다시 시도
          </Button>
        </div>
      ) : (
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <div className="space-y-1.5">
            <Label htmlFor="password">새 비밀번호</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="최소 8자"
              autoFocus
              autoComplete="new-password"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="confirm">비밀번호 확인</Label>
            <Input
              id="confirm"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder="비밀번호를 다시 입력하세요"
              autoComplete="new-password"
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button type="submit" className="w-full" disabled={loading || !password || !confirm}>
            {loading ? "저장 중..." : "비밀번호 설정 후 로그인"}
          </Button>
        </form>
      )}
    </PrtsAuthShell>
  );
}
