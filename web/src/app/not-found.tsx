"use client";

import Link from "next/link";

import { PrtsBrand } from "@/components/prts-brand";
import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-6 px-6 text-center">
      <PrtsBrand />
      <div className="prts-section-code">404 / LOCATION NOT FOUND</div>
      <h1 className="font-semibold text-2xl">이 페이지를 찾을 수 없습니다</h1>
      <p className="text-muted-foreground text-sm">페이지가 이동되었을 수 있습니다. 연구 터미널로 돌아가 계속 작업하세요.</p>
      <Link prefetch={false} replace href="/chat">
        <Button variant="outline">연구 터미널로 돌아가기 ↗</Button>
      </Link>
    </main>
  );
}
