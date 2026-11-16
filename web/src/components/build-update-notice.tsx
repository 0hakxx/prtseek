"use client";

import { useEffect, useState } from "react";

const pageBuild = process.env.NEXT_PUBLIC_BUILD_ID;

export function BuildUpdateNotice() {
  const [changed, setChanged] = useState(false);
  useEffect(() => {
    if (!pageBuild) return;
    let disposed = false;
    let active: AbortController | undefined;
    const check = async () => {
      if (document.visibilityState === "hidden" || active) return;
      const controller = new AbortController();
      active = controller;
      const timeout = setTimeout(() => controller.abort(), 5000);
      try {
        const response = await fetch("/api/health", { cache: "no-store", signal: controller.signal });
        if (!response.ok) return;
        const health = await response.json();
        if (!disposed && typeof health.build_id === "string" && health.build_id) {
          setChanged(health.build_id !== pageBuild);
        }
      } catch { /* A temporary outage is not an update. */ }
      finally { clearTimeout(timeout); active = undefined; }
    };
    void check();
    const interval = setInterval(check, 30000);
    window.addEventListener("focus", check);
    document.addEventListener("visibilitychange", check);
    return () => {
      disposed = true;
      clearInterval(interval);
      active?.abort();
      window.removeEventListener("focus", check);
      document.removeEventListener("visibilitychange", check);
    };
  }, []);
  if (!changed) return null;
  return (
    <div role="status" data-testid="build-update-notice" className="fixed top-3 left-1/2 z-[100] flex w-[min(36rem,calc(100%-2rem))] -translate-x-1/2 items-center gap-3 border border-border bg-card p-3 text-sm text-card-foreground shadow-lg">
      <span className="flex-1">程序已更新，当前页面仍是旧版。请先保存输入内容。</span>
      <button type="button" className="shrink-0 border border-border px-3 py-2 hover:bg-accent" onClick={() => window.location.reload()}>刷新加载新版</button>
    </div>
  );
}
