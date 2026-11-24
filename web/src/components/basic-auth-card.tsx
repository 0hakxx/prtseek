"use client";

import { useCallback, useEffect, useState } from "react";

import { ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";

export function BasicAuthCard() {
  const [enabled, setEnabled] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [passwordSet, setPasswordSet] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setError("");
    try {
      const settings = await api.basicAuthSettings();
      setEnabled(settings.enabled);
      setUsername(settings.username);
      setPasswordSet(settings.password_set);
      setLoaded(true);
    } catch (e) {
      setError(`읽기 실패: ${(e as Error).message}`);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setError("");
    try {
      const settings = await api.setBasicAuthSettings({ enabled, username: username.trim(), password });
      setEnabled(settings.enabled);
      setUsername(settings.username);
      setPasswordSet(settings.password_set);
      setPassword("");
      toast.success(settings.enabled ? "HTTP Basic Auth 가 켜졌습니다. 현재 브라우저는 접근을 유지합니다" : "HTTP Basic Auth 가 꺼졌습니다");
    } catch (e) {
      setError(`저장 실패: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheckIcon className="size-4" />
          HTTP Basic Auth
        </CardTitle>
        <CardDescription>
          켜면 방문자는 먼저 브라우저 기본 계정·비밀번호 인증을 통과한 뒤 prtseek 로그인 페이지로 들어갑니다. 기본 꺼짐이며, 저장하면 즉시 적용됩니다.
        </CardDescription>
      </CardHeader>
      <form onSubmit={save}>
        <CardContent>
          <FieldGroup>
            <Field orientation="horizontal">
              <FieldLabel htmlFor="basic-auth-enabled">접근 인증 사용</FieldLabel>
              <Switch
                id="basic-auth-enabled"
                checked={enabled}
                onCheckedChange={setEnabled}
                disabled={!loaded || saving}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="basic-auth-username">인증 사용자 이름</FieldLabel>
              <Input
                id="basic-auth-username"
                autoComplete="off"
                value={username}
                required={enabled}
                disabled={!loaded || saving}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="prtseek 로그인 계정과는 별개"
              />
              <FieldDescription>콜론을 포함할 수 없고 최대 128 바이트입니다.</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="basic-auth-password">인증 비밀번호</FieldLabel>
              <Input
                id="basic-auth-password"
                type="password"
                autoComplete="new-password"
                value={password}
                required={enabled && !passwordSet}
                disabled={!loaded || saving}
                onChange={(e) => setPassword(e.target.value)}
                placeholder={passwordSet ? "설정됨, 비우면 기존 비밀번호 유지" : "최소 8자, 최대 72 바이트"}
              />
              <FieldDescription>prtseek 로그인 비밀번호와 별개입니다. 공인망 접근에는 HTTPS 를 사용하세요.</FieldDescription>
            </Field>
            <FieldDescription>
              저장해도 현재 브라우저 접근은 끊기지 않습니다. 시크릿 창으로 인증 팝업을 확인할 수 있습니다. 브라우저가 자격 증명을 캐시할 수 있습니다. 끈 뒤에도 계정·비밀번호는 유지되어 다시 켜기 편합니다.
            </FieldDescription>
            {error ? (
              <p role="alert" className="text-destructive text-sm">
                {error}
              </p>
            ) : null}
          </FieldGroup>
        </CardContent>
        <CardFooter className="mt-4 gap-2">
          <Button type="submit" disabled={!loaded || saving}>
            {saving ? "저장 중…" : "접근 인증 저장"}
          </Button>
          {!loaded && error ? (
            <Button type="button" variant="outline" onClick={load}>
              다시 로드
            </Button>
          ) : null}
        </CardFooter>
      </form>
    </Card>
  );
}
