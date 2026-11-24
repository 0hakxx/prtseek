import type { NextRequest } from "next/server";
import { NextResponse } from "next/server";

const AUTH_PAGES = ["/login", "/setup"];

export async function proxy(request: NextRequest) {
  // Mock demo: 실제 로그인 없이 모든 페이지를 통과시킴(클라이언트 auth 가드도 통과시킴).
  if (process.env.NEXT_PUBLIC_MOCK === "1") return NextResponse.next();

  // Pages served by Next dev must pass the same gate as the embedded UI.
  // Only send credentials to a loopback backend; never follow redirects.
  // Remote dev backends remain usable when their optional gate is disabled.
  let gate: Response;
  try {
    const backend = new URL(process.env.AUTOPENTEST_API ?? "http://localhost:8787");
    if (!["http:", "https:"].includes(backend.protocol) || backend.username || backend.password) {
      return new NextResponse("인증 백엔드 주소 설정이 유효하지 않습니다", { status: 503 });
    }
    const local = ["localhost", "127.0.0.1", "[::1]"].includes(backend.hostname);
    gate = await fetch(new URL("/api/basic-auth/check", backend), {
      headers: local
        ? {
            cookie: request.headers.get("cookie") ?? "",
            authorization: request.headers.get("authorization") ?? "",
            "x-forwarded-proto": request.nextUrl.protocol.replace(":", ""),
          }
        : {},
      credentials: "omit",
      cache: "no-store",
      redirect: "error",
      signal: AbortSignal.timeout(5000),
    });
    if (!local && gate.status === 401) {
      return new NextResponse("원격 백엔드에 HTTP Basic Auth 가 켜져 있습니다. 내장 프론트엔드를 통해 이 서비스에 접근하세요", { status: 503 });
    }
  } catch {
    return new NextResponse("인증 서비스를 일시적으로 사용할 수 없습니다. 잠시 후 다시 시도하세요", { status: 503 });
  }
  if (!gate.ok) {
    const headers = new Headers({ "Cache-Control": "no-store", "Content-Type": "text/plain; charset=utf-8" });
    const challenge = gate.headers.get("www-authenticate");
    if (challenge) headers.set("WWW-Authenticate", challenge);
    return new NextResponse(gate.status === 401 ? "HTTP Basic Auth 인증을 완료한 뒤 페이지를 새로 고치세요" : "인증 서비스를 일시적으로 사용할 수 없습니다", {
      status: gate.status,
      headers,
    });
  }
  const finish = (response: NextResponse) => {
    const cookie = gate.headers.get("set-cookie");
    if (cookie) response.headers.append("Set-Cookie", cookie);
    response.headers.set("Cache-Control", "no-store");
    return response;
  };

  const { pathname } = request.nextUrl;
  const token = request.cookies.get("prtseek_token")?.value ?? request.cookies.get("artex_token")?.value;
  const isAuthPage = AUTH_PAGES.some((p) => pathname === p || pathname.startsWith(`${p}/`));

  // 미로그인 → 로그인 페이지로 이동
  if (!token && !isAuthPage) {
    return finish(NextResponse.redirect(new URL("/login", request.url)));
  }

  // 로그인 상태에서 로그인/초기화 페이지 접근 시 → 메인 화면으로 이동
  if (token && isAuthPage) {
    return finish(NextResponse.redirect(new URL("/chat", request.url)));
  }

  return finish(NextResponse.next());
}

export const config = {
  // Next.js 내부 라우트, API 라우트, favicon, public/ 아래 정적 파일(이미지, 폰트 등 포함) 건너뛰기
  matcher: [
    "/((?!_next/static|_next/image|favicon\\.ico|api/|.*\\.(?:png|jpg|jpeg|gif|webp|svg|ico|woff2?|ttf|otf)$).*)",
  ],
};
