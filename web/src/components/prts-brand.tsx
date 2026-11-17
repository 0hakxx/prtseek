import { cn } from "@/lib/utils";

export function PrtsMark({ className, light = false }: { className?: string; light?: boolean }) {
  return <span aria-hidden="true" className={cn("prts-mark size-9 shrink-0", light && "prts-mark-light", className)} />;
}

export function PrtsBrand({
  className,
  compact = false,
  light = false,
}: {
  className?: string;
  compact?: boolean;
  light?: boolean;
}) {
  return (
    <span className={cn("prts-brand", className)}>
      <PrtsMark light={light} />
      {!compact && (
        <span className="prts-brand-copy">
          <strong>prtseek</strong>
          <span>{"TACTICAL // CTF CONTROL"}</span>
        </span>
      )}
    </span>
  );
}
