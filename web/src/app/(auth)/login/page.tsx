"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { PrtsAuthShell } from "@/components/prts-auth-shell";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // 약관 끝까지 스크롤해야(스크롤 없이도 전체가 보이는 경우 포함) 「동의」를 누를 수 있다.
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 열 때 초기화하고, 내용이 한 화면도 안 되어 스크롤을 트리거할 수 없는 경우를 처리한다.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // 이미 로그인했으면 메인 화면으로 바로 들어간다(정적 내보내기에서는 middleware 가 이 이동을 대신하지 않음).
    const token = auth.getToken();
    if (token) {
      // localStorage 에 자격 증명이 여전히 있지만 cookie 가 사라졌을 수 있다. 먼저 동기화한 뒤 완전히 새 요청을 보내,
      // 서버 가드나 라우트 캐시가 아직 checking 상태인 로그인 페이지로 이동을 되돌리는 것을 피한다.
      auth.setToken(token);
      window.location.replace("/chat");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("백엔드 서비스에 연결할 수 없습니다"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("먼저 《사용 안내》를 읽고 동의하세요");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("prtseek", password);
      auth.setToken(token);
      window.location.replace("/chat");
    } catch {
      setError("사용자 이름 또는 비밀번호가 틀렸습니다");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        로그인 상태를 확인하는 중…
      </div>
    );
  }

  return (
    <>
      <PrtsAuthShell>
        <div className="space-y-3">
          <h2 className="text-2xl font-medium tracking-tight">터미널 접근</h2>
          <p className="text-sm leading-relaxed text-muted-foreground">접근 비밀번호를 입력해 연구를 계속하세요.</p>
        </div>
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <div className="space-y-1.5">
            <Label htmlFor="username">사용자 이름</Label>
            <Input id="username" value="prtseek" readOnly className="bg-muted text-muted-foreground" />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="password">비밀번호</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="비밀번호를 입력하세요"
              autoFocus
              autoComplete="current-password"
            />
          </div>
          <div className="flex items-start gap-2">
            <Checkbox
              id="agree-terms"
              checked={agreed}
              onCheckedChange={(v) => setAgreed(v === true)}
              className="mt-0.5"
            />
            <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
              읽고 동의했습니다
              <button
                type="button"
                onClick={() => setTermsOpen(true)}
                className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
              >
                《사용 안내》
              </button>
            </Label>
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
            {loading ? "로그인 중..." : "로그인"}
          </Button>
        </form>
      </PrtsAuthShell>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">prtseek 사용 안내 및 면책 조항</DialogTitle>
              <p className="text-xs text-muted-foreground">
                버전 v1.0 · 시행일 2026-09-18 · 로그인 전에 아래 모든 조항을 완전히 읽으세요
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              이 《사용 안내 및 면책 조항》(이하 "본 성명")은 귀하와 prtseek
              프로젝트 작성자 및 기여자 사이에 본 소프트웨어 사용에 관해 맺는 약정입니다. 사용하기 전에 각 조항의 내용을, 특히 굵은 글씨나 색 블록으로 표시된 면책, 책임 제한, 금지 조항을 신중히 읽고 충분히 이해하시기 바랍니다.
              <span className="font-medium text-foreground">
                {" "}
                귀하가 본 소프트웨어를 다운로드, 설치, 접근하거나 어떤 방식으로든 사용하면, 본 성명의 모든 구속을 읽고 이해하고 받아들이는 데 동의한 것으로 봅니다.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                제1조 · 정의와 오픈 소스 라이선스
              </h4>
              <p className="pl-7">
                본 소프트웨어(prtseek)는 GNU Affero General Public License
                v3.0(AGPL-3.0)으로 배포되는 오픈 소스 프로그램입니다. 귀하는 그 라이선스에 따라 본 소프트웨어를 자유롭게 사용, 복제, 수정, 배포할 수 있습니다. 다만 모든 파생 저작물(네트워크를 통해 제3자에게 제공하는 온라인 서비스 포함)도 똑같이
                AGPL-3.0 라이선스로 공개하고 사용자에게 해당하는 완전한 소스 코드를 공개해야 합니다. AGPL-3.0 의 전체 조항은 함께 제공되는 LICENSE 파일을 기준으로 합니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                제2조 · 사용 허가 범위
              </h4>
              <p className="pl-7">
                본 소프트웨어는 개인 학습, 코드 연구, 보안 기술 원리 탐구, 그리고 귀하가 직접 구축한 로컬 격리 환경에서의 기술 검증 용도로만 제공되며, 학습, 학술 연구, 코드 리뷰 등 비공격적·비파괴적 용도에 적합합니다. 본 조에서 명확히 허가한 경우를 제외하고, 귀하는 본 소프트웨어를 다른 어떤 목적으로도 사용할 수 없습니다.
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                제3조 · 금지 행위
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  어떤 웹사이트, 온라인 서비스, 타인이나 제3자가 소유한 네트워크 연결 시스템에 대해서도 스캔, 탐지, 익스플로잇, 공격을 하는 것을 엄격히 금지합니다(권한을 얻었는지, 귀하의 자산인지와 무관);
                </li>
                <li>본 소프트웨어를 실제 침투 테스트, 공방 대결, 레드/블루 훈련, 운영 환경에 사용하는 것을 엄격히 금지합니다;</li>
                <li>본 소프트웨어를 불법 침입, 데이터 절취, 갈취, 서비스 거부(DoS/DDoS) 또는 어떤 파괴적·범죄적 활동에 사용하는 것을 엄격히 금지합니다;</li>
                <li>본 소프트웨어 및 그 출력에 포함된 어떤 저작권, 라이선스, 보안 안내 정보도 제거, 변조, 우회하는 것을 엄격히 금지합니다;</li>
                <li>귀하가 속한 국가나 지역의 법률, 법규, 규제 규정을 위반하는 어떤 행위도 엄격히 금지합니다.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                제4조 · 지식 재산권
              </h4>
              <p className="pl-7">
                본 소프트웨어의 저작권 및 관련 지식 재산권은 프로젝트 작성자와 기여자에게 있으며, AGPL-3.0
                라이선스가 정한 범위에서 귀하에게 해당 권리를 부여합니다. 그 라이선스가 명시적으로 부여한 권리를 제외하고, 본 성명은 명시적이든 묵시적이든 귀하에게 다른 어떤 권리도 부여하지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                제5조 · 데이터와 개인정보
              </h4>
              <p className="pl-7">
                본 소프트웨어는 직접 배포할 수 있는 오픈 소스 프로그램이며, 작성자는 어떤 중앙 집중식 서비스도 운영하지 않고 귀하의 사용 데이터를 수집하거나 업로드하지도 않습니다. 사용 과정에서 귀하가 생성, 처리, 접촉하는 모든 데이터는 귀하가 직접 관리하고 그 적법성과 안전에 책임을 집니다. 데이터 처리가 부적절해 발생하는 어떤 결과도 귀하가 직접 부담합니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                제6조 · 준법과 법적 책임
              </h4>
              <p className="pl-7">
                귀하는 속한 국가나 지역의 네트워크 보안, 데이터 보안과 개인정보 보호, 컴퓨터 범죄 등에 관한 모든 법률 법규를 스스로 준수해야 합니다(중국 본토에서는 《네트워크 안전법》《데이터 안전법》《개인정보 보호법》 및 관련 사법 해석을 포함하되 이에 국한되지 않음).
                <span className="font-medium text-foreground">
                  {" "}
                  귀하가 위 법률 법규나 본 성명의 약정을 위반해 발생하는 모든 법적 책임과 결과는 귀하 본인이 단독으로 부담하며, 본 소프트웨어 작성자 및 기여자와는 무관합니다.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                제7조 · 면책 조항과 책임 제한
              </h4>
              <p className="pl-7">
                본 소프트웨어는 "있는 그대로(AS IS)" 및 "이용 가능한 상태(AS
                AVAILABLE)"로 제공되며, 상품성, 특정 목적 적합성, 정확성, 비침해에 대한 보증을 포함하되 이에 국한되지 않는 어떤 명시적·묵시적 보증도 수반하지 않습니다. 적용 법률이 허용하는 최대 범위에서, 본 소프트웨어 작성자 및 기여자는 본 소프트웨어의 사용 또는 사용 불가(사용 방식의 적절성과 무관)로 발생한 직접적, 간접적, 우발적, 특별, 결과적 손해(데이터 손실, 시스템 손상, 업무 중단, 이익 손실, 법적 분쟁을 포함하되 이에 국한되지 않음)에 대해 어떤 책임도 지지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                제8조 · 조항 변경과 최종 해석
              </h4>
              <p className="pl-7">
                작성자는 법률 법규나 프로젝트 발전의 필요에 따라 수시로 본 성명을 갱신할 권리가 있으며, 갱신된 버전은 프로젝트와 함께 공개되어 공개일부터 효력이 생깁니다. 귀하가 본 소프트웨어를 계속 사용하면 개정된 조항을 받아들인 것으로 봅니다. 법률이 허용하는 범위에서 본 성명의 최종 해석권은 프로젝트 작성자에게 있습니다. 본 성명의 어떤 조항이 무효로 판정되어도 나머지 조항의 효력에는 영향을 주지 않습니다.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "모든 조항을 확인했습니다" : "조항을 끝까지 스크롤한 뒤 확인하세요"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                모든 조항을 읽고 동의했습니다
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
