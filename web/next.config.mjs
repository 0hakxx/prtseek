import { fileURLToPath } from "node:url";

// 정적 내보내기: `NEXT_EXPORT=1 next build` 가 순수 정적 디렉터리를 web/out 에 산출하며, 바로
// nginx web 루트 디렉터리에 넣어 실행할 수 있다. 개발(next dev)은 이 변수를 설정하지 않고 /api 리버스 프록시와 핫 리로드를 유지한다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: 전체 사이트가 mock 을 쓰며 백엔드가 없고 /api 리버스 프록시가 필요 없다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 디렉터리의 lockfile 이 루트 디렉터리 추론과 리소스 경로 생성에 영향을 주는 것을 피한다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 로컬 네트워크 IP 에서 dev 리소스(HMR)에 접근하도록 허용. 필요에 따라 추가/삭제.
  // dev 단계에서 임의의 IPv4 출처가 /_next/* 와 HMR 에 접근하도록 열어 둠(로컬 네트워크 IP 가 바뀌어도 영향 없음).
  // 주의: Next 는 보안상 맨 "*" 를 금지하므로 구간 와일드카드를 써야 함. "*.*.*.*" 는 임의의 IPv4 를 매칭.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 순수 정적 내보내기: Node 런타임 없음. 이미지는 최적화되지 않음. 라우트마다 <route>/index.html 산출.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: 백엔드가 없어 /api 리버스 프록시가 필요 없음.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/* 를 Go 백엔드로 리버스 프록시(기본 :8787, AUTOPENTEST_API 로 재정의 가능).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
