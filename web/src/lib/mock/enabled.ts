// Mock 스위치. 빌드 시 주입되는 공개 변수(NEXT_PUBLIC_ 접두사만 브라우저에서 읽힘).
// Vercel 에서 NEXT_PUBLIC_MOCK=1 로 설정하면 전체 사이트가 mock 로 동작, 백엔드 불필요.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
