"use client";
import { ModelEditor } from "@/components/model-settings";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useEffect, useState } from "react";

export function PersonalOnboarding() {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    const configure = () => setOpen(true);
    window.addEventListener("prtseek-configure-model", configure);
    return () => window.removeEventListener("prtseek-configure-model", configure);
  }, []);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>모델 설정</DialogTitle>
          <DialogDescription>모델 설정을 입력하면 대화를 시작할 수 있습니다.</DialogDescription>
        </DialogHeader>
        <ModelEditor initial onSaved={() => setOpen(false)} />
      </DialogContent>
    </Dialog>
  );
}
