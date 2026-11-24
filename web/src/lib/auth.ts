const TOKEN_KEY = "prtseek_token";
const LEGACY_TOKEN_KEY = "artex_token"; // Read existing installations without forcing a new login.
const COOKIE_MAX_AGE = 7 * 24 * 60 * 60; // 7 일(초)

export interface CurrentUser {
  id: string;
  name: string;
  username: string;
  email: string;
  avatar: string;
  role: string;
}

export const auth = {
  getToken(): string | null {
    if (typeof window === "undefined") return null;
    // Mock demo: 실제 로그인 없이 가짜 token 을 반환해 라우트 가드를 통과시키고 바로 메인 화면으로 들어간다.
    return (
      localStorage.getItem(TOKEN_KEY) ??
      localStorage.getItem(LEGACY_TOKEN_KEY) ??
      (process.env.NEXT_PUBLIC_MOCK === "1" ? "mock-demo" : null)
    );
  },

  setToken(token: string): void {
    localStorage.setItem(TOKEN_KEY, token);
    // cookie 를 동기로 써서 Next.js middleware 가 서버 측에서 읽게 한다
    document.cookie = `${TOKEN_KEY}=${encodeURIComponent(token)}; path=/; max-age=${COOKIE_MAX_AGE}; SameSite=Lax`;
  },

  clearToken(): void {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(LEGACY_TOKEN_KEY);
    document.cookie = `${LEGACY_TOKEN_KEY}=; path=/; max-age=0`;
    document.cookie = `${TOKEN_KEY}=; path=/; max-age=0`;
  },

  // JWT payload 의 sub 필드에서 현재 사용자를 해석한다. 표시용일 뿐 서명 검증은 하지 않는다.
  getCurrentUser(): CurrentUser | null {
    const token = this.getToken();
    if (!token) return null;
    try {
      const parts = token.split(".");
      if (parts.length !== 3) return null;
      // base64url → base64
      const payload = JSON.parse(atob(parts[1].replace(/-/g, "+").replace(/_/g, "/")));
      const subject: string = payload.sub ?? "prtseek";
      const username = subject.toLowerCase() === "artex" ? "prtseek" : subject;
      return { id: "1", name: username, username, email: "", avatar: "", role: "operator" };
    } catch {
      return null;
    }
  },
};
