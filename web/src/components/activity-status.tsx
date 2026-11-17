"use client";

import * as React from "react";

import { Check, CirclePause, Terminal } from "lucide-react";

import { elapsedLabel } from "@/lib/ctf-status";
import { hideResearchEvidenceAnnotation } from "@/lib/research-evidence";
import { activityElapsed, activityStatus } from "@/lib/tool-execution-state";
import type { Activity } from "@/lib/types";

import styles from "./chat-workspace.module.css";

function useClock(live: boolean) {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    if (!live) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [live]);
  return now;
}

export function ToolElapsed({ from, to, live }: { from?: string; to?: string; live: boolean }) {
  const now = useClock(live);
  const elapsed = activityElapsed(from, live ? now : to ? Date.parse(to) : Number.NaN);
  return elapsed == null ? null : <span className={styles.elapsed}>{elapsedLabel(elapsed)}</span>;
}

export function ActivityStatusBar({
  activity,
  live,
  state,
  outcome,
  startedMs,
  connectionError,
  loading,
}: {
  activity: Activity[];
  live: boolean;
  state?: string;
  outcome?: string;
  startedMs?: number;
  connectionError?: boolean;
  loading?: boolean;
}) {
  const status = activityStatus(activity, live, state, outcome, connectionError, loading);
  const now = useClock(status.live);
  const start =
    status.kind === "tool" ? status.since : startedMs && startedMs > 0 ? new Date(startedMs).toISOString() : undefined;
  const elapsed = activityElapsed(start, now);
  const quiet = activityElapsed(status.updatedAt, now);
  const timestamp =
    status.updatedAt && Number.isFinite(Date.parse(status.updatedAt)) ? new Date(status.updatedAt) : null;
  const action = hideResearchEvidenceAnnotation(status.action);
  return (
    <section className={styles.activityStatus} data-run-state={status.kind} aria-label="当前执行状态">
      <span className={styles.statusOrb} data-live={status.live} aria-hidden="true">
        {status.kind === "completed" ? (
          <Check className="size-3.5" />
        ) : status.kind === "tool" ? (
          <Terminal className="size-3.5" />
        ) : !status.live ? (
          <CirclePause className="size-3.5" />
        ) : null}
      </span>
      <div className={styles.statusContent}>
        <div className={styles.statusHeading}>
          <strong role="status">{status.label}</strong>
          {status.live && elapsed != null && (
            <span className={styles.elapsed}>
              {status.kind === "tool" ? "工具已用 " : "本轮已用 "}
              {elapsedLabel(elapsed)}
            </span>
          )}
          {timestamp && (
            <time
              dateTime={timestamp.toISOString()}
              title={timestamp.toLocaleString("zh-CN")}
              className={styles.statusUpdated}
            >
              {status.live && quiet != null
                ? `距上次回传 ${elapsedLabel(quiet)}`
                : `更新于 ${timestamp.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`}
            </time>
          )}
        </div>
        <p title={action}>{action}</p>
      </div>
    </section>
  );
}
