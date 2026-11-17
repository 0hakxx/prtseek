import type { Activity } from "./types";

export type ToolExecutionState = "running" | "unreturned" | "succeeded" | "failed";

export interface ToolExecutionContext {
  live: boolean;
  turnBoundary: number;
  workerBoundaries: ReadonlyMap<string, number>;
}

// A missing result is only live inside the current turn. Terminal messages are
// worker-scoped so one parallel worker finishing does not stop another's tool.
export function createToolExecutionContext(activity: readonly Activity[], live = false): ToolExecutionContext {
  let turnBoundary = Number.NEGATIVE_INFINITY;
  const workerBoundaries = new Map<string, number>();
  for (const step of activity) {
    if (step.inherited) continue;
    if (step.kind === "user" || step.kind === "round") {
      turnBoundary = Math.max(turnBoundary, step.seq);
    } else if (step.kind === "result" || step.kind === "intent") {
      workerBoundaries.set(
        step.worker,
        Math.max(workerBoundaries.get(step.worker) ?? Number.NEGATIVE_INFINITY, step.seq),
      );
    }
  }
  return { live, turnBoundary, workerBoundaries };
}

export function toolExecutionState(
  use: Activity | undefined,
  result: Activity | undefined,
  context: ToolExecutionContext,
): ToolExecutionState {
  if (result) return result.is_error ? "failed" : "succeeded";
  if (!use || use.inherited || !context.live) return "unreturned";
  const boundary = Math.max(context.turnBoundary, context.workerBoundaries.get(use.worker) ?? Number.NEGATIVE_INFINITY);
  return use.seq > boundary ? "running" : "unreturned";
}

export interface ActivityStatus {
  kind: "model" | "tool" | "unreturned" | "completed" | "interrupted" | "idle" | "disconnected" | "loading";
  label: string;
  action: string;
  live: boolean;
  since?: string;
  updatedAt?: string;
}

export function activityStatus(
  activity: readonly Activity[],
  live: boolean,
  state = "",
  outcome = "",
  connectionError = false,
  loading = false,
): ActivityStatus {
  const steps = activity
    .filter((step) => !step.inherited)
    .slice()
    .sort((a, b) => a.seq - b.seq);
  const latest = steps.at(-1);
  const updatedAt = latest?.ts || undefined;
  if (connectionError)
    return {
      kind: "disconnected",
      label: "状态连接中断",
      action: "正在重新连接，最后记录保留在下方",
      live: false,
      updatedAt,
    };
  if (loading) return { kind: "loading", label: "读取状态", action: "正在读取会话记录", live: false };
  const context = createToolExecutionContext(steps, live);
  const results = new Map(
    steps.filter((step) => step.kind === "tool_result" && step.tool_use_id).map((step) => [step.tool_use_id, step]),
  );
  const tools = steps.filter((step) => step.kind === "tool_use");
  const pending = tools.filter(
    (step) => toolExecutionState(step, results.get(step.tool_use_id), context) === "running",
  );
  if (live && pending.length) {
    const current = pending.at(-1)!;
    const tool = current.tool || "工具";
    let action = current.summary.startsWith(tool) ? current.summary.slice(tool.length).trim() : current.summary;
    try {
      const input = JSON.parse(action);
      action = input.description || input.command || action;
    } catch {
      /* The API summary can be truncated. */
    }
    return {
      kind: "tool",
      label: `${tool} 运行中`,
      action: action || "等待工具返回结果",
      live: true,
      since: current.ts,
      updatedAt,
    };
  }
  if (live)
    return {
      kind: "model",
      label: "模型处理中",
      action:
        latest?.kind === "tool_result" ? "已收到工具结果，继续处理" : "服务正在运行，等待下一条模型消息或工具调用",
      live: true,
      since: latest?.ts,
      updatedAt,
    };
  if (["interrupted", "cancelled", "failed", "timed_out", "budget_exhausted", "stalled", "blocked"].includes(state)) {
    const labels: Record<string, string> = {
      interrupted: "已中断",
      cancelled: "已停止",
      failed: "执行失败",
      timed_out: "已超时",
      budget_exhausted: "预算已用尽",
      stalled: "进展停滞",
      blocked: "等待外部条件",
    };
    return { kind: "interrupted", label: labels[state], action: "可查看最后记录并继续处理", live: false, updatedAt };
  }
  if (outcome === "verified")
    return { kind: "completed", label: "目标已完成", action: "结果与关键步骤见下方", live: false, updatedAt };
  if (state === "completed" || latest?.kind === "result")
    return { kind: "completed", label: "本轮已结束", action: "可继续提问或查看记录", live: false, updatedAt };
  const boundary = context.turnBoundary;
  const unmatched = tools.filter((step) => step.seq > boundary && !results.has(step.tool_use_id));
  if (unmatched.length)
    return {
      kind: "unreturned",
      label: "工具未返回结果",
      action: "本轮已停止，最后一次调用没有结果记录",
      live: false,
      updatedAt,
    };
  return { kind: "idle", label: "待命", action: "发送消息开始处理", live: false, updatedAt };
}

export function activityElapsed(from: string | undefined, to: number): number | null {
  const start = from ? Date.parse(from) : Number.NaN;
  return Number.isFinite(start) && Number.isFinite(to) && to >= start ? to - start : null;
}
