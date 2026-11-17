export const ENTRY_ROLES = [
  { key: "solve", name: "解题", description: "直接分析题目；遇到难点时并行研究，得到结果立即交付。" },
  { key: "writeup", name: "写 WP", description: "根据已有会话和文件整理解题过程、关键证据与复现步骤。" },
] as const;

export function entryRoleLabel(key: string): string {
  if (["solve", "triage", "command", "solver", "mainagent", "planner", "worker", "goals", "ctf_triage", "ctf_planner", "ctf_worker", "ctf_verifier"].includes(key))
    return "解题";
  if (["writeup", "ctf_writeup", "reporter"].includes(key)) return "写 WP";
  const worker = /^work#(\d+)$/.exec(key);
  if (worker) return `解题 #${worker[1]}`;
  return key;
}
