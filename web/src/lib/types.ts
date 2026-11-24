// prtseek domain model — types used across the UI.
// Derived from the functional spec (section 7: 핵심 데이터 형태).

type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
type EngineMode = "exploring" | "paused" | "stalled" | "idle";

// Keep these limits aligned with db.MaxTaskRemarkRunes and db.MaxTaskWorkerCount.
export const MAX_TASK_REMARK_RUNES = 2000;
export const MAX_TASK_WORKER_COUNT = 50;

export interface Task {
  distinct_findings?: FindingDistinctStats;
  id: string;
  remark?: string;
  worker_count?: number; // 이 작업의 Worker 수. 0/생략=전역 기본값 사용
  name?: string; // 선택적 작업 이름. 비어 있음/생략=이름 없음, 표시할 때 설명으로 대체
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 등록된 취약점 수(심각도별)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 자산 커버리지 기능 스위치(생성 시 정함, 기본 켜짐). false=커버리지 계산/표시 안 함
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  remark?: string; // 사전 설정 메모
  category_id?: number | null; // 사전 설정 분류. null/생략=없음
  intercept_rules?: AssetInterceptRuleInput[]; // 사전 설정된 작업 단위 차단/허용 규칙
  worker_count?: number; // 사전 설정 Worker 수. 0/생략=전역 기본값 사용
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// ---- New unified asset model (new backend) ----
type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 힘 기반 「자산 커버리지 그래프」의 노드 하나. key 는 고유: 자산="a:<id>", 회사="c:<id>",
// 자산 행이 없는 루트 도메인="r:<domain>". in_scope=false 인 것은 연결용 회색 컨텍스트 노드.
interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 어떤 자산이 이 작업 탐색 그래프에서 연결된 의도/사실/발견(커버리지 그래프 노드 서랍용).
interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// 작업 테스트 범위의 한 항목(커버리지 분모 + 권한 경계).
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // 백엔드가 companies 를 JOIN 해 해석. kind=company 일 때만 값이 있음
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 기업을 추가할 때 제출하는 구조화 자산 범위 규칙.
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 자산 범위 쓰기 결과. errors 는 이번 제출에서 유효하지 않은 행. warnings 는 이번 제출과 무관하지만,
// 다만 귀속 결과가 예상과 맞지 않게 되는 기존 데이터 문제(예: ip 필드에 호스트명이 저장된 자산)를 일으킨다.
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 회사 자산 범위 규칙의 한 항목(귀속의 유일한 진실 출처).
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain 일 때 값이 있음
  net?: string; // kind=ip|cidr 일 때 값이 있음
  value?: string; // kind=icp|keyword 일 때 백엔드가 직접 반환할 수 있음
  raw: string; // 원본 사용자 입력. 표시와 되채움에 사용
  reason?: string;
}

// 기업: type=company 인 자산 노드 + 아이콘 + 자산 수 + 자산 범위 규칙.
export interface Company {
  id: number;
  name: string;
  logo?: string; // 원격 아이콘 URL. 비어 있으면 프론트엔드가 이름 첫 글자를 씀
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 의도 가짜 삭제(state='deleted') 시의 삭제 사유
}

// 상황판 한 페이지: 생성 순서로 페이지를 나눈 노드 + 이 페이지와 관련된 간선 + 간선 반대쪽 끝의 노드(refs, id 로 색인),
// 이렇게 하면 각 상황 보고가 「어디서 왔고 무엇을 산출했는가」를 설명할 수 있어 그래프 전체를 가져오지 않아도 된다.
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // 노드 id → 그 노드가 고정한 자산(상황판을 펼칠 때 함께 표시. 이 페이지의 노드와 그 이웃을 포함).
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 목표 관리 카드용 목표(백엔드가 이미 payload 를 text/vulnclass 로 나눔).
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 제약 관리 카드용 작업 제약(allow=허용 / deny=금지).
type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 취약점 처리 상태: 처리 대기 / 처리 중 / 확인됨 / 처리됨 / 수정됨 / 오탐 / 무시 / 중복 / 위험 수용.
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset 은 한 취약점에 바인딩된 자산(백엔드에서 이미 label 을 미리 렌더링).
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  case_id?: string;
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 독립 findings 테이블의 행 id, 상태 갱신의 핸들(작업 안의 오래된 노드는 없을 수 있음)
  vulnclass: string;
  name?: string; // 취약점 이름. 비어 있으면 vulnclass 로 대체 표시
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 상세 보고서(Markdown). 상세 인터페이스에서만 반환하며 목록에서는 비어 있음
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage 는 발견 목록의 서버 측 페이지 응답.
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 선택적 작업 이름. 비어 있음/생략=이름 없음
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats 는 발견 전체 표 집계(통계 카드 + 취약점 유형 드롭다운)이며, 서버가 계산하고 페이지에 영향받지 않는다.
export interface FindingStats {
  distinct?: FindingDistinctStats;
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption 은 발견 페이지 「작업별」 필터 드롭다운의 한 항목: 취약점이 있는 작업(설명이 비어 있으면 작업이 삭제되었다는 뜻,
// 프론트엔드는 id 로 대체 표시)과 그 취약점 건수.
interface FindingTaskOption {
  id: string | number;
  name?: string; // 선택적 작업 이름. 비어 있음/생략=이름 없음
  description: string;
  count: number;
}

// FindingQuery 는 발견 목록의 페이지/필터/정렬 파라미터.
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // 작업 id. "all"/비어 있음 = 작업으로 필터하지 않음
  query?: string;
  sort?: "severity" | "time";
  // 자산 트리 노드 key. 노드 하나 선택 = 그 하위 트리 전체 선택. 비어 있음 = 자산으로 필터하지 않음.
  assetScope?: string;
}

// ---- Findings by asset (자산 뷰) ----
type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode 는 자산 트리의 노드 하나. key 형식은 a:<id>(자산), c:<id>(기업),
// r:<domain>(DB 에 자산 행이 없는 루트 도메인), __none__(자산 미연결).
interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // 그 자산에 직접 걸린 발견 수
  total: number; // 자손 포함, 발견 기준 중복 제거
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// ---- Activity / sessions ----
type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment 는 한 번 업로드한 파일: path 는 그 대화/작업 작업 디렉터리(즉 agent 의 CWD) 기준 상대 경로.
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 절대 경로(scope=staging 임시 업로드 시 반환. 작업을 만들기 전에 설명에 씀)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 스케줄링, 사용자 정의 agent 만) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 예약: N 초마다(0=예약 안 함)
  on_finding: boolean; // 임의 작업에서 finding 발견 시 트리거
  on_goal_met: boolean; // 임의 작업에서 목표 달성 시 트리거
  on_task_timeout: boolean; // 임의 작업 시간 초과 시 트리거
  on_tool_call: boolean; // 선택한 도구가 호출됨(실행 완료) 시 트리거
  on_task_create: boolean; // 임의 작업 생성 시 트리거
  interval_message: string; // 각 트리거 조건의 독립 사용자 메시지
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call 로 선택한 도구 key(최소 하나)
  last_fire?: string;
}

export interface TaskSchedule {
  id: number;
  name: string;
  remark?: string;
  enabled: boolean;
  timezone_mode: "beijing" | "system";
  timezone?: string;
  schedule_type: "once" | "weekly";
  run_date?: string;
  end_date?: string;
  weekdays: number[];
  start_time: string;
  end_time?: string;
  start_immediately: boolean;
  status: "waiting" | "running" | "outside" | "completed" | "error" | "paused";
  last_window_key?: string;
  last_transition_at?: string;
  last_error?: string;
  next_start?: string;
  next_end?: string;
  run_count?: number;
  last_run_at?: string;
  task_ids: number[];
  history?: Array<{
    id: number;
    task_id?: number;
    action: string;
    success: boolean;
    message: string;
    created_at: string;
  }>;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  mode?: "standard" | "ctf";
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  reasoning_effort?: string;
  effective_profile_id?: number;
  effective_model?: string;
  effective_reasoning_effort?: string;
  response_language?: string;
  background_status?: string;
  background_paused?: boolean;
  background_task_id?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 분류 일괄 변경의 작업별 결과. 실패는 작업이 이미 삭제된 경우뿐이며, 분류 자체의 쓰기는 원자적이다.
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Security ----
interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  credential_agent_enabled: boolean;
  credential_traffic_binding: boolean;
  task_intelligence_enabled: boolean; // 기본 꺼짐. 꺼짐이면 정보 도구나 프롬프트를 주입하지 않음
  agent_traffic_binding: boolean; // Agent 가 트래픽 증거를 자동 바인딩, 기본 꺼짐. 수동 바인딩에는 영향 없음
  llm_record: boolean; // LLM 녹화 스위치(기본 꺼짐). 꺼짐이면 어떤 LLM 호출도 기록하지 않음
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 독립 출구 프록시(http/https/socks5). 검색 엔드포인트 접근에 쓰며, 트래픽을 기록하는 MITM 프록시와는 무관. 비어 있음=직접 연결.
  web_search_proxy?: string;
  // 전역 출구 프록시(http/https/socks5, user:pass 포함 가능). 모든 대상 트래픽이 이것을 탐. 트래픽 캡처를 켜면
  // MITM 상위로 쓰고, 캡처를 끄면 agent 의 bash/WebFetch 에 직접 주입. 비어 있음=직접 연결.
  global_proxy?: string;
  python_interpreter?: string; // 사용자 정의 스크립트 도구의 python 인터프리터 경로(비어 있음=런타임 감지)
  workers?: number; // 동시 작업 agent 수(기본 3). 이후 시작되는 작업에 적용
  worker_pools: WorkerPool[];
  // 작업 동시 실행 상한: 동시에 「실행 중」인 작업 수 상한. 꺼짐=무제한. 켜면 새 작업이 상한을 넘을 때 대기열에 들어가고 빈자리가 생기면 자동 시작.
  task_concurrency_enabled?: boolean; // 기본 false
  task_concurrency_limit?: number; // 켜면 기본 5
  // LLM 폴링(장애 조치). 기본 꺼짐. 켜면 「모델 미지정」 agent 가 현재 설정을 사용할 수 없을 때
  // (잔액 부족/key 만료/속도 제한/서비스 이상) 자동으로 다음 설정으로 바꿈.
  llm_pool_enabled?: boolean; // 기본 false
  // 지정된 설정을 바인딩한 agent/작업이 실패할 때 폴링 체인으로도 돌아갈지. 기본 false = 바인딩하면 독점.
  llm_pool_bind_fallback?: boolean;
  // 작업 제약 주입 범위(기본 모두 켜짐): 작업의 allow/deny 제약을 해당 agent 의 시스템 프롬프트에 붙임.
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 실험 기능: noa 모델 주도 컨텍스트 압축(기본 꺼짐). 켜면 플랫폼이 접속한 네 종류 agent(planner/
  // worker/메인 agent/대화)의 컨텍스트 압축을 noa 가 맡아 내장 compaction 을 대체. run 마다 한 번 읽으며,
  // 이후 시작되는 run 에 적용.
  noa_compaction?: boolean;
  // ---- 취약점 IM 푸시(채널 자체는 독립 리소스, /api/notify/* 참고. 여기에는 전역 설정 세 항목만) ----
  notify_enabled?: boolean; // 푸시 전체 스위치, 기본 켜짐. 유지보수 기간 원클릭 지혈용
  notify_public_base_url?: string; // 취약점 상세 링크의 외부 접근 주소. 비어 있음=메시지에 링크 없음
  notify_digest_interval_min?: number; // 요약 모드 주기(분), 기본 30
}

interface WorkerPool {
  profile_id: string;
  workers: number;
}

// ---- 취약점 IM 푸시 ----

// NotificationFilter 는 채널의 필터 조건이며, 필드는 모두 선택이고 생략하면 필터하지 않음.
// 백엔드는 모든 필드를 검증하지 않음: 설정이 잘못되면 「적중」으로 처리(덜 보내느니 더 보냄).
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 비어 있음=제한 없음. 비어 있지 않으면 취약점이 속한 작업과 교집합 요구
  asset_ids?: number[]; // 비어 있음=제한 없음. 비어 있지 않으면 취약점이 고정한 자산과 교집합 요구
  vulnclass_include?: string[]; // 비어 있음=모두 받음. 비어 있지 않으면 취약점 유형이 어느 키워드든 적중 요구(대소문자 구분 안 하는 부분 문자열)
  vulnclass_exclude?: string[]; // 어느 키워드든 적중하면 제외(제외가 포함보다 우선)
  on_status_change?: boolean; // 취약점 처리 상태 변경 이벤트도 받을지
}

// NotificationChannel 은 채널 인스턴스 하나. config 의 필드는 kind 에 따라 다르며,
// 자격 증명 필드는 읽을 때 "__masked__" 로 시작하는 마스킹 값으로 바뀐다——그대로 되돌려 보내면 「바꾸지 않음」을 뜻한다.
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys 는 백엔드가 채널 유형에 따라 주며, 프론트엔드는 이를 바탕으로 비밀번호 상자와 「비우면 바꾸지 않음」 안내를 렌더링한다.
  // 어떤 채널 지식도 하드코딩하지 않는다.
  secret_keys: string[];
}

// NotificationKind 는 /api/notify/meta 가 반환하는 채널 유형 메타데이터.
interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery 는 전달 기록 하나이며, 전달 이력과 실패 재전송에 쓴다.
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 사고 켜기/끄기(thinking.type): ""=보내지 않음(기본) | "disabled"=끔 | "enabled"=켬
  thinking_type?: string;
  // 사고 강도: ""=보내지 않음(기본) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 폴링 순위: 클수록 먼저 선택됨. 활성 설정은 항상 체인 맨 앞이며 이 값과 무관.
  priority?: number;
  // true = 장애 조치 대상이 되지 않음(여전히 agent/작업이 명시적으로 바인딩해 쓸 수 있음).
  pool_exclude?: boolean;
  // true(기본) = 스트리밍(SSE) | false = 진짜 비스트리밍(stream:false, 한 번에 반환).
  streaming?: boolean;
  // 한 번의 응답 출력 상한(token). 0 = 이 필드를 보내지 않고 서버 기본값에 맡김.
  // context_window_k 와 구분할 것: 후자는 모델 전체 용량이며 로컬에서 압축 임계값에만 쓴다.
  max_tokens?: number;
  // 상한에 어떤 요청 필드 이름을 쓸지, format="openai" 일 때만 의미 있음:
  // ""=max_tokens(기본) | "max_completion_tokens"(OpenAI 추론 모델은 이것만 받음)
  max_tokens_field?: string;
  // 사용자 정의 대화 헤더 이름: 비어 있지 않으면 요청마다 이 HTTP 헤더를 붙이며, 헤더 값=현재 대화/의도의 session id.
  // ""=보내지 않음. session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 게이트웨이용.
  session_header_key?: string;
  // 이 설정의 재시도 재정의(연결/빈 응답/같은 provider 안전 구간). 비움/모두 0 = 전역 정책을 따름.
  retry?: LLMRetryOverride;
}

// ---- LLM 재시도 정책 ----
// 한 계층 재시도의 두 손잡이. 둘 다 「0 = 설정 안 됨」:
//   attempts    0=기본 횟수 사용 | -1=해당 계층 재시도 끔 | >0=재시도 횟수
//   interval_ms 0=기본 지수 백오프 사용 | >0=이 고정 밀리초 간격으로 바꿔 사용
interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 단일 LLM 설정이 재정의할 수 있는 세 계층(모두 「엔드포인트를 따라가는」 재시도).
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 연결 재시도: 연결 재설정/시간 초과/429/5xx, 스트림 시작 전
  empty: LLMRetryRule; // 빈 응답 재시도: 완료되었지만 내용이 전혀 없음(openai 형식만)
  stream: LLMRetryRule; // 같은 provider 안전 구간 재시도: 출력을 넘기기 전의 스트림 끊김 재생
}

// 전역 정책 = 위 세 계층의 기본값 + 전역에만 있는 두 계층:
//   breaker 폴링 서킷 브레이커(attempts=몇 번 연속 일시적 실패하면 차단, interval_ms=고정 냉각 시간)
//   intent  의도 재실행(worker 가 model_error 로 끝난 뒤 의도 전체를 다시 실행)
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 폴링(장애 조치) ----
// 설정 하나가 폴링 체인에서 차지하는 위치와 상태. state:
//   ok       정상
//   degraded 연속 실패가 있지만 차단 임계값에 도달하지 않음
//   tripped  이미 차단됨, 냉각 기간에 건너뛰어짐(cooldown_secs 는 남은 초)
interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 현재 활성 설정인지(항상 체인 맨 앞)
  excluded: boolean; // pool_exclude: 폴링에 참여하지 않음
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 내장은 goals/planner/mainagent/worker. 사용자 정의는 사용자가 정한 key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 바인딩된 LLM 설정. null/absent = 작업/대화/전역을 따름
  max_turns?: number; // 0 = 무제한
  run_seconds?: number; // worker 단일 실행 벽시계 상한(초). 0 = 무제한
  web_search?: boolean; // 웹 검색 사용 여부(시스템 전역 스위치의 제어를 받음)
  interactive_shell?: boolean; // 대화형 shell 사용 여부(영구 PTY 세션 도구군)
  // P3 트리거 후처리 정책(사용자 정의 agent 에서만 의미 있음)
  trigger_run_mode?: "serial" | "parallel"; // 직렬 대기열 / 트리거마다 각자 대화를 하나씩 동시 실행
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial 만: 같은 작업 병합 / 전부 병합 / 병합 안 함
  trigger_max_parallel?: number; // parallel 만: agent 별 동시 실행 상한. 0=무제한
  // 바인딩 수(목록 인터페이스에서만 반환): 보이는 MCP / 보이는 Skill / 바인딩 도구
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 바인딩할 수 있는 LLM 설정 후보(「기본 모델」 드롭다운용). 현재 바인딩은 agent.llm_profile_id 참고
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 저장된 마무리 프롬프트(비어 있음=내장 기본값 사용)
  wrapup_default?: string; // 내장 기본 마무리 프롬프트(자리표시자/기본값 복원)
  wrapup_max_turns?: number; // 저장된 마무리 턴 수(0=내장 기본값 사용)
  wrapup_max_turns_default?: number; // 내장 기본 마무리 턴 수("0=기본 N" 안내용)
  // 작업 단위 시간 초과 마무리 문구(worker/planner 만. task_timeout_wrapup_supported=true 일 때만 이 구역을 표시)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 호출 통계(skill_usage 장부). 한 번도 호출된 적 없는 skill: calls=0, last_used 없음.
  calls: number;
  tasks: number; // 그것을 로드한 작업 수(chat 대화는 집계하지 않음)
  usage_agents: string[]; // 그것을 로드한 agent key
  last_used?: string;
}

// SkillCall 은 한 번의 Skill() 호출(단일 skill 의 최근 호출 목록).
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = 작업이 아닌 경우(대화 세션)
  session_id: string;
  args_len: number;
}

// MissingSkill 은 지목되었지만 존재하지 않는 skill —— "쓰려 했지만 없음"인 빈틈.
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (내장 도구 목록) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // 사용자 정의 도구 유형
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // 사용자 정의 도구 실행 사양(kind!=builtin)
  deferred?: boolean; // schema 지연(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
type InterceptAction = "allow" | "deny" | "ask";
type InterceptMatchTarget = "tool_name" | "tool_input";
type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules(자산 차단: 전역 블랙리스트) ----
type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action 은 작업 단위 규칙에만 쓴다: block=차단(테스트 금지) allow=허용(화이트리스트).
type AssetInterceptAction = "block" | "allow";

// 작업 단위 자산 차단/허용 규칙의 입력 항목(작업 생성, 작업 상세 편집에 사용).
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 전역 규칙에는 이 필드가 없음(항상 차단). 작업 단위 규칙은 block/allow 를 구분
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "manual" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 규칙 message 또는 모델 판정 이유(모델 판정은 [모델] 접두사가 붙음)
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: 모델 대비 승인(차단 규칙에 하나도 걸리지 않았을 때만 모델이 판단)의 전역 설정.
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 활성/기본 설정을 따름
  prompt: string; // 판정 프롬프트. GET 에서 설정되지 않았으면 백엔드가 내장 템플릿 전문을 채움
  timeout_seconds: number; // 모델 승인 호출마다의 시간 초과
  retry_count: number; // 승인 실패 후 추가 재시도 횟수, 0 = 재시도 안 함
  retry_interval_ms: number; // 승인 재시도 간격(밀리초), 0 = 즉시 재시도
  fail_action: "allow" | "ask" | "deny"; // 모델 오류/시간 초과/파싱 불가 시의 대비
  ask_timeout_seconds: number; // 모델이 ask 로 수동 전환한 뒤의 승인 대기 시간 초과
  ask_timeout_action: "allow" | "deny"; // 승인 시간 초과 후의 기본 동작
  incomplete_script_action: "ask" | "model"; // 스크립트 스냅샷이 불완전할 때: 수동 검토 또는 모델 검토 계속
}

// JudgeUsage: 모델 대비 승인(judge 채널)의 누적 token 사용량 + 최근 N 일의 일별 시퀀스.
interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "manual" | "unknown";
  q?: string;
  from?: string;
  to?: string;
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 자산 동기화 (ScopeSentry 데이터 소스) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 단일 도구의 호출 통계(/commands/stats). errors 는 그중 실패한 횟수.
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider 가 실제로 송수신한 HTTP 원문: 요청은 buildBody() 가 보낸 완전한 body(도구
  // schema 포함), 응답은 원본 SSE 프레임. 위의 request_body/response_body 는 정규화된 뷰이며,
  // 도구 schema 와 tool_use 블록을 버린 것. 오래된 기록은 비어 있음.
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  script_capture_status?: "not_applicable" | "complete" | "incomplete";
  script_files?: {
    path: string;
    source?: "file" | "command";
    sha256?: string;
    bytes?: number;
    content?: string;
    complete?: boolean;
    truncated?: boolean;
    read_error?: string;
  }[];
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

export interface RepeaterSendRequest {
  method: string;
  http_version?: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
  timeout_ms?: number;
  insecure_skip_verify?: boolean;
  use_recording_proxy?: boolean;
}

export interface RepeaterSendResponse {
  status: number;
  status_text: string;
  proto: string;
  headers: Record<string, string[]>;
  body: string;
  body_truncated: boolean;
  body_bytes: number;
  elapsed_ms: number;
  url: string;
  error?: string;
}

export interface UpdateSettings {
  proxy: string;
}

/** GET /api/update/check —— 현재 버전과 GitHub 최신 정식 버전의 비교 결과. */
export interface UpdateCheck {
  /** 현재 실행 중인 버전. 개발 빌드는 "dev" 또는 git describe 의 접미사 형태. */
  current: string;
  /** 실행 형태. docker 에서 교체는 컨테이너 쓰기 가능 계층에만 작용하며, 컨테이너를 다시 만들면 이미지 버전으로 돌아감. */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 롤백할 수 있는 이전 버전(prtseek.old)이 있는지. */
  has_backup: boolean;
  /** 이번 시작 시 자동 업데이트 부트스트랩의 결론(교체 실패 / 롤백됨 등). 아무 일도 없으면 비어 있음. */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub 조회 실패 시 원인을 제시하며, 이때 아래 필드는 아무것도 없음. */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 현재 플랫폼에 해당하는 배포 패키지 이름, 그리고 그 Release 가 정말로 그것을 가지고 있는지. */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 양쪽 버전 번호를 비교할 수 있는지. 개발 빌드는 false 이며, 이때 원클릭 업데이트가 비활성화됨. */
  comparable?: boolean;
  /** comparable 이 false 일 때의 설명. */
  reason?: string;
}

interface FindingDistinctStats {
  total: number;
  reports: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  unassessed: number;
}
interface FindingCase {
  id: string;
  task_id: string | null;
  title: string;
  reason: string;
  severity: Severity | "";
  severity_reason: string;
  report?: string;
  version: number;
  report_version: number;
  active: boolean;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  created_at: string;
}
interface FindingCaseListRow {
  task_name?: string;
  task_description?: string;
  assets?: FindingAsset[];
  asset_count?: number;
  last_found_at?: string;
  case?: FindingCase | null;
  finding?: Finding;
  matched_ids: number[];
}
export interface FindingCasePage {
  matching_reports?: number;
  items: FindingCaseListRow[];
  total: number;
  page: number;
  page_size: number;
  stats: FindingDistinctStats;
}
interface FindingCaseEvent {
  id: string;
  action: string;
  actor: string;
  reason: string;
  finding_ids: number[];
  created_at: string;
}
export interface FindingCaseDetail {
  case: FindingCase;
  events: FindingCaseEvent[];
}
export interface FindingCaseSuggestion {
  id: string;
  task_id: string;
  left_id: string;
  right_id: string;
  finding_ids: string[];
  title: string;
  reason: string;
  state: string;
}
export interface FindingCaseReviewRun {
  conversation_id: number;
  task_id: string;
  state: "queued" | "running" | "done" | "failed";
  error: string;
}

type IntelligenceKind = "credential" | "lead" | "sensitive_data";
interface CredentialAccount {
  label?: string;
  username?: string;
  password?: string;
  token?: string;
  cookie?: string;
  endpoint?: string;
  protocol?: string;
}
interface CredentialTraffic {
  id: string;
  url: string;
  method: string;
  status: number;
  side: string;
  excerpt: string;
  accounts: number[];
}
interface CredentialAnalysis {
  state: "queued" | "processing" | "ready" | "error";
  method?: "rules" | "agent";
  accounts: CredentialAccount[] | null;
  traffic: CredentialTraffic[] | null;
  traffic_state: string;
  traffic_note?: string;
  note?: string;
  error?: string;
  model?: string;
  input_tokens: number;
  output_tokens: number;
  processed_at?: string;
}
export interface CredentialTrafficDetail {
  reference: CredentialTraffic;
  req?: string;
  resp?: string;
  unavailable: boolean;
}
export interface TaskIntelligence {
  credentials?: CredentialAnalysis;
  source_session?: string;
  id: number;
  task_id: number;
  node_id?: number;
  kind: IntelligenceKind;
  title: string;
  content: string;
  evidence: string;
  confidence: "observed" | "inferred";
  status: string;
  asset_ids: number[];
  worker: string;
  created_at: string;
  updated_at: string;
  inherited: boolean;
}
export interface IntelligencePage {
  credential_agent_enabled: boolean;
  credential_traffic_binding: boolean;
  items: TaskIntelligence[];
  total: number;
  counts: Record<IntelligenceKind, number>;
  enabled: boolean;
}
