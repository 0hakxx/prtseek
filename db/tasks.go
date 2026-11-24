package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Task is a row in the task registry (1:1 with an exploration).
type Task struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"` // 선택적 작업 이름. 비어 있음=이름 없음
	Remark        string     `json:"remark"`
	CategoryID    *int64     `json:"category_id,omitempty"`
	CategoryName  string     `json:"category_name,omitempty"`
	Pinned        bool       `json:"pinned"`
	PinnedAt      *time.Time `json:"pinned_at,omitempty"`
	Description   string     `json:"description"`
	Goal          string     `json:"goal"`
	ExplorationID int64      `json:"exploration_id"`
	Status        string     `json:"status"`
	Paused        bool       `json:"paused"`
	Queued        bool       `json:"queued"`
	QueuedAt      *time.Time `json:"queued_at,omitempty"`
	QueueMode     string     `json:"queue_mode,omitempty"`
	LLMProfileID  *int64     `json:"llm_profile_id,omitempty"`
	// Task-level ordered LLM chain. LLMProfileID remains the compatibility alias
	// for ActiveLLMProfileID while older API clients still send one profile id.
	LLMProfileIDs      []int64    `json:"llm_profile_ids,omitempty"`
	ActiveLLMProfileID *int64     `json:"active_llm_profile_id,omitempty"`
	LLMChainRevision   int64      `json:"-"`
	LLMFailoverState   string     `json:"llm_failover_state,omitempty"`
	LLMFailoverReason  string     `json:"llm_failover_reason,omitempty"`
	SourceTaskIDs      []int64    `json:"source_task_ids,omitempty"`
	CompanyIDs         []int64    `json:"company_ids,omitempty"`
	ParentRef          string     `json:"parent_ref,omitempty"` // 상위 작업 id(오케스트레이션 spawn 기록. 비어 있음=최상위)
	CreatedAt          time.Time  `json:"created_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"` // 최종 상태(done/failed/timeout)에 들어간 시각. 최종 상태가 아니면 nil
	// 작업 단위 시간 초과(docs/任务级超时与收尾设计.md 참고).
	TimeoutSeconds int        `json:"timeout_seconds"`        // 0=시간 무제한
	FirstRunAt     *time.Time `json:"first_run_at,omitempty"` // 처음 실제로 실행을 시작한 시각(created_at 아님). nil=아직 실행 안 함
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`  // = first_run_at + timeout_seconds. nil=무제한이거나 실행 안 함
	// planner 하트비트 트리거 간격(초): 지난 plan 종료/작업 시작 이후 이 값만큼 지났고 그동안 트리거가 없었으면 → 한 턴을 트리거한다.
	// 하한=기본값=300(5min)이며, 그보다 낮으면 모두 300 으로 올린다(CreateTask 에서 정규화). docs/planner-trigger-impl-plan.md 참고
	PlanHeartbeatSeconds int `json:"plan_heartbeat_seconds"`
	// CoverageEnabled 는 「자산 커버리지 기능」의 전체 스위치다(기본 true). false 이면: 테스트
	// 커버리지를 계산/표시하지 않고, task_scope(source=auto)를 자동 누적하지 않으며, agent 에게 add_task_scope/
	// list_untested_assets 를 열어 주지 않고, 상황에 coverage 블록을 주입하지 않는다(scope 필드는 그대로 유지). company 연결
	// (task_scope kind=company)은 이 스위치와 무관하며 절대 영향을 받지 않는다. db/task_scope.go 참고.
	CoverageEnabled bool `json:"coverage_enabled"`
	// WorkerCount is the per-task concurrent work-agent count. 0 means use the
	// global settings.workers default when the engine starts this task.
	WorkerCount int `json:"worker_count"`
}

// TaskDeleteResult reports optional related-data cleanup performed in the same
// transaction as the task/exploration delete.
type TaskDeleteResult struct {
	AssetsDeleted     int64
	AssetsDetached    int64
	FindingsDeleted   int64
	LLMRecordsDeleted int64
}

// TaskDeletePreparation is produced inside the SQLite deletion transaction
// after asset and anchor writers have been excluded. Prepare callbacks may use
// TrafficHosts to stage an external traffic deletion before SQLite commits.
type TaskDeletePreparation struct {
	ExplorationID int64
	TrafficHosts  []string
}

// IsTerminal reports whether a task status is a terminal (finished) state.
// 단일 진실 출처로, 곳곳에 흩어진 done/failed 하드코딩 판정을 대체한다.
func IsTerminal(status string) bool {
	return status == "done" || status == "failed" || status == "timeout"
}

// CreateTask creates an exploration + task in one transaction and returns the task.
// timeoutSeconds is the task-level wall-clock budget (0 = 시간 무제한); deadline_at is
// stamped later at first real run (see engine), not here.
// MinPlanHeartbeatSeconds 는 planner 하트비트 간격의 하한 = 기본값 = 10min 이다.
// 그보다 낮으면(기본 0 / 음수 / 잘못 설정한 작은 값 포함) 모두 10min 으로 올려 planner 가 폭주하지 않게 한다.
const MinPlanHeartbeatSeconds = 600

// MaxTaskSourceCount bounds the amount of live inherited context one task can
// pull into every planner/main-agent prompt. Inheritance is intentionally direct
// only; keeping the fan-in bounded also prevents a single create request from
// multiplying graph and asset-context queries without limit.
const MaxTaskSourceCount = 8

// MaxTaskCompanyCount bounds the number of company asset scopes attached to a
// task. Company scopes are prompt context, and their currently attributed
// assets are snapshotted into the task at creation time.
const MaxTaskCompanyCount = 32

const taskCompanyAssetSource = "company"

var (
	ErrTaskCompanyIDsInvalid = errors.New("invalid task company ids")
	ErrTaskCompanyNotFound   = errors.New("task company not found")
)

// NormalizeTaskCompanyIDs validates IDs and removes duplicates while retaining
// the user's first-seen order.
func NormalizeTaskCompanyIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, min(len(ids), MaxTaskCompanyCount))
	normalized := make([]int64, 0, min(len(ids), MaxTaskCompanyCount))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: company id must be positive", ErrTaskCompanyIDsInvalid)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
		if len(normalized) > MaxTaskCompanyCount {
			return nil, fmt.Errorf("%w: got more than %d unique companies", ErrTaskCompanyIDsInvalid, MaxTaskCompanyCount)
		}
	}
	return normalized, nil
}

func normalizeHeartbeat(sec int) int {
	if sec < MinPlanHeartbeatSeconds {
		return MinPlanHeartbeatSeconds
	}
	return sec
}

// MaxTaskWorkerCount is the hard upper bound for a per-task worker override.
const MaxTaskWorkerCount = 50

// MaxTaskRemarkRunes is the shared limit for task and template remarks.
const MaxTaskRemarkRunes = 2000

// normalizeWorkerCount converts negative per-task worker overrides to 0.
// The upper bound is enforced separately by validateWorkerCount; 0 means
// "use the global settings.workers default".
func normalizeWorkerCount(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func validateWorkerCount(n int) error {
	if n > MaxTaskWorkerCount {
		return fmt.Errorf("Worker 실행 수는 최대 %d 입니다", MaxTaskWorkerCount)
	}
	return nil
}

func (d *DB) CreateTask(description, goal string, llmProfileID *int64, timeoutSeconds, planHeartbeatSeconds int) (*Task, error) {
	var ids []int64
	if llmProfileID != nil {
		ids = []int64{*llmProfileID}
	}
	return d.CreateTaskWithOptions(description, goal, TaskCreateOptions{
		LLMProfileIDs: ids, TimeoutSeconds: timeoutSeconds, PlanHeartbeatSeconds: planHeartbeatSeconds,
	})
}

// TaskCreateOptions contains the task data that must be committed atomically
// with the task/exploration row.
type TaskCreateOptions struct {
	// PersonalHandoff atomically binds a personal conversation to this task.
	PersonalHandoff      *PersonalHandoffInput
	Schedule             *TaskScheduleInput
	Name                 string // 선택적 작업 이름. 비어 있음=이름 없음
	CategoryID           *int64
	SourceTaskIDs        []int64
	CompanyIDs           []int64
	LLMProfileIDs        []int64
	TimeoutSeconds       int
	PlanHeartbeatSeconds int
	// CoverageEnabled 는 「자산 커버리지 기능」 스위치다. nil=기본 켜짐(true)이므로, 이 스위치에 관심 없는 생성
	// 경로(오케스트레이션 spawn, 예전 API)는 원래 동작을 따른다. web 에서 작업을 만들 때만 명시적으로 false 를 넘겨 끌 수 있다.
	CoverageEnabled *bool
	// WorkerCount 는 이 작업에서 동시에 실행하는 작업 Agent 수다. 0/생략=전역 기본값 사용. 상한 50.
	WorkerCount int
	// Remark 는 생성할 때 붙일 수 있는 작업 메모다(최대 2000자). 비어 있음=메모 없음.
	Remark string
	// InterceptRules 는 작업 단위 자산 차단 규칙이며, 생성할 때 작업과 같은 트랜잭션에서 task_intercept_rules 에 쓴다.
	InterceptRules []TaskInterceptRuleInput
}

// CreateTaskWithOptions creates an exploration, task, direct source relations,
// and the ordered task LLM chain in one transaction.
func (d *DB) CreateTaskWithOptions(description, goal string, opts TaskCreateOptions) (*Task, error) {
	if len(opts.SourceTaskIDs) > MaxTaskSourceCount {
		return nil, fmt.Errorf("too many source tasks: got %d, maximum is %d", len(opts.SourceTaskIDs), MaxTaskSourceCount)
	}
	companyIDs, err := NormalizeTaskCompanyIDs(opts.CompanyIDs)
	if err != nil {
		return nil, err
	}
	opts.CompanyIDs = companyIDs
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockPersonalHandoff(tx, opts.PersonalHandoff); err != nil {
		return nil, err
	}
	var expID int64
	if err := tx.QueryRow(`INSERT INTO explorations(description, goal) VALUES ($1,$2) RETURNING id`, description, goal).Scan(&expID); err != nil {
		return nil, err
	}
	// origin fact: the exploration graph's root, a KindFact node (state='origin')
	// holding the original task description. Goals/intents/findings descend from
	// it, and the seeded target asset is anchored to it as lineage (the asset graph
	// is global and shared, not isolated per task). Being a fact (not a special 'begin' kind) lets every
	// intent uniformly connect to a fact node, including the first ones.
	originPayload, _ := json.Marshal(map[string]any{
		"summary":     "작업 시작점: " + description + "; 목표: " + goal,
		"description": description,
		"goal":        goal,
	})
	if _, err := tx.Exec(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'fact', $2, 0, 'origin', 'system')`, expID, string(originPayload)); err != nil {
		return nil, err
	}
	if opts.TimeoutSeconds < 0 {
		opts.TimeoutSeconds = 0
	}
	opts.PlanHeartbeatSeconds = normalizeHeartbeat(opts.PlanHeartbeatSeconds)
	coverageEnabled := opts.CoverageEnabled == nil || *opts.CoverageEnabled
	opts.WorkerCount = normalizeWorkerCount(opts.WorkerCount)
	if err := validateWorkerCount(opts.WorkerCount); err != nil {
		return nil, err
	}
	opts.Remark = strings.TrimSpace(opts.Remark)
	if utf8.RuneCountInString(opts.Remark) > MaxTaskRemarkRunes {
		return nil, fmt.Errorf("메모는 최대 %d 자입니다", MaxTaskRemarkRunes)
	}
	var categoryName string
	if opts.CategoryID != nil {
		if *opts.CategoryID <= 0 {
			return nil, fmt.Errorf("%w: category id must be positive", ErrTaskCategoryInvalid)
		}
		if err := tx.QueryRow(`SELECT name FROM task_categories WHERE id=$1`, *opts.CategoryID).Scan(&categoryName); err != nil {
			if err == sql.ErrNoRows {
				return nil, ErrTaskCategoryNotFound
			}
			return nil, err
		}
	}
	var active *int64
	if len(opts.LLMProfileIDs) > 0 {
		id := opts.LLMProfileIDs[0]
		active = &id
	}
	t := &Task{
		Name: opts.Name, CategoryID: opts.CategoryID, CategoryName: categoryName,
		Description: description, Goal: goal, ExplorationID: expID,
		LLMProfileID: active, ActiveLLMProfileID: active,
		LLMProfileIDs:  append([]int64(nil), opts.LLMProfileIDs...),
		SourceTaskIDs:  append([]int64(nil), opts.SourceTaskIDs...),
		CompanyIDs:     append([]int64(nil), opts.CompanyIDs...),
		TimeoutSeconds: opts.TimeoutSeconds, PlanHeartbeatSeconds: opts.PlanHeartbeatSeconds,
		CoverageEnabled: coverageEnabled, WorkerCount: opts.WorkerCount, Remark: opts.Remark,
	}
	if err := tx.QueryRow(`
INSERT INTO tasks(name, remark, category_id, description, goal, exploration_id, llm_profile_id, active_llm_profile_id, timeout_seconds, plan_heartbeat_seconds, coverage_enabled, worker_count)
VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8,$9,$10,$11)
RETURNING id, status, paused, created_at`, opts.Name, opts.Remark, opts.CategoryID, description, goal, expID, active, opts.TimeoutSeconds, opts.PlanHeartbeatSeconds, coverageEnabled, opts.WorkerCount).Scan(&t.ID, &t.Status, &t.Paused, &t.CreatedAt); err != nil {
		return nil, err
	}
	if err := insertTaskRelations(tx, t.ID, opts.SourceTaskIDs); err != nil {
		return nil, err
	}
	if err := insertTaskCompanies(tx, t.ID, opts.CompanyIDs); err != nil {
		return nil, err
	}
	if err := insertTaskLLMProfiles(tx, t.ID, opts.LLMProfileIDs); err != nil {
		return nil, err
	}
	if err := insertTaskInterceptRules(tx, t.ID, opts.InterceptRules); err != nil {
		return nil, err
	}
	if len(opts.LLMProfileIDs) == 0 {
		t.LLMFailoverState = "default"
	} else {
		t.LLMFailoverState = "ready"
	}
	if opts.Schedule != nil {
		in, err := normalizeScheduleInput(*opts.Schedule)
		if err != nil {
			return nil, err
		}
		in.TaskIDs = []int64{t.ID}
		if _, err = insertTaskSchedule(tx, in); err != nil {
			return nil, err
		}
		if !in.StartImmediately {
			if _, err = tx.Exec(`UPDATE tasks SET paused=true,schedule_paused=true,queue_mode='bootstrap' WHERE id=$1`, t.ID); err != nil {
				return nil, err
			}
			t.Paused = true
			t.QueueMode = "bootstrap"
		}
	}
	if err := insertPersonalHandoff(tx, t, opts.PersonalHandoff); err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

func insertTaskCompanies(tx *sql.Tx, taskID int64, companyIDs []int64) error {
	if len(companyIDs) == 0 {
		return nil
	}
	// Company scope edits rebuild assets.company_id. Serialize the creation-time
	// snapshot with those edits so the task sees one committed attribution state.
	result, err := tx.Exec(`INSERT INTO task_scope(task_id,kind,company_id,source,reason)
 SELECT $1,'company',c.id,'manual','작업 생성 시 기업 연결'
 FROM json_each($2) requested JOIN companies c ON c.id=requested.value
 ORDER BY requested.key`, taskID, companyIDs)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	inserted := int(count)
	if inserted != len(companyIDs) {
		return fmt.Errorf("%w: one or more companies do not exist", ErrTaskCompanyNotFound)
	}

	// Updating task_ids fires sync_task_asset_links, which first creates generic
	// source rows. The provenance upsert must therefore run afterwards so the
	// company name and creation-time reason remain visible to operators.
	if _, err := tx.Exec(`
UPDATE assets
SET task_ids=CASE
    WHEN $1 IN (SELECT value FROM json_each(task_ids)) THEN task_ids
    ELSE collection_append(task_ids, $1)
END
WHERE company_id IN (SELECT value FROM json_each($2))`, taskID, companyIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT $1, asset.id, $3, '작업 생성 시 기업 연결: ' || company.name
FROM assets asset
JOIN companies company ON company.id=asset.company_id
WHERE asset.company_id IN (SELECT value FROM json_each($2))
  AND $1 IN (SELECT value FROM json_each(asset.task_ids))
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source=EXCLUDED.source,
    source_summary=EXCLUDED.source_summary,
    source_node_id=NULL`, taskID, companyIDs, taskCompanyAssetSource); err != nil {
		return err
	}
	return nil
}

func insertTaskRelations(tx *sql.Tx, taskID int64, sourceIDs []int64) error {
	seen := map[int64]bool{}
	for _, sourceID := range sourceIDs {
		if sourceID <= 0 || sourceID == taskID || seen[sourceID] {
			return fmt.Errorf("invalid or duplicate source task id %d", sourceID)
		}
		seen[sourceID] = true
		res, err := tx.Exec(`INSERT INTO task_relations(task_id, source_task_id)
SELECT $1, id FROM tasks WHERE id=$2 AND deleted_at IS NULL`, taskID, sourceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("source task %d not found", sourceID)
		}
	}
	return nil
}

func insertTaskLLMProfiles(tx *sql.Tx, taskID int64, profileIDs []int64) error {
	seen := map[int64]bool{}
	for position, profileID := range profileIDs {
		if profileID <= 0 || seen[profileID] {
			return fmt.Errorf("invalid or duplicate LLM profile id %d", profileID)
		}
		seen[profileID] = true
		if _, err := tx.Exec(`INSERT INTO task_llm_profiles(task_id, profile_id, position) VALUES ($1,$2,$3)`, taskID, profileID, position); err != nil {
			return err
		}
	}
	return nil
}

const taskCols = `id, COALESCE(name,''), remark, category_id,
COALESCE((SELECT category.name FROM task_categories category WHERE category.id=tasks.category_id),''),
description, goal, exploration_id, status, paused, queued, queued_at, COALESCE(queue_mode,''), llm_profile_id, active_llm_profile_id, COALESCE(parent_ref,''), pinned_at, created_at, completed_at, COALESCE(timeout_seconds,0), COALESCE(plan_heartbeat_seconds,300), COALESCE(coverage_enabled,true), COALESCE(worker_count,0), first_run_at, deadline_at`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	if err := sc.Scan(&t.ID, &t.Name, &t.Remark, &t.CategoryID, &t.CategoryName, &t.Description, &t.Goal, &t.ExplorationID, &t.Status, &t.Paused, &t.Queued, &t.QueuedAt, &t.QueueMode, &t.LLMProfileID, &t.ActiveLLMProfileID, &t.ParentRef, &t.PinnedAt, &t.CreatedAt, &t.CompletedAt, &t.TimeoutSeconds, &t.PlanHeartbeatSeconds, &t.CoverageEnabled, &t.WorkerCount, &t.FirstRunAt, &t.DeadlineAt); err != nil {
		return nil, err
	}
	t.Pinned = t.PinnedAt != nil
	return &t, nil
}

// SetParentRef records a task's parent task id (오케스트레이션 agent spawn_task 연결).
func (d *DB) SetParentRef(id int64, parentRef string) error {
	_, err := d.Exec(`UPDATE tasks SET parent_ref=NULLIF($2,'') WHERE id=$1`, id, parentRef)
	return err
}

// ListTasks returns alive tasks with pinned tasks first, then newest ids.
func (d *DB) ListTasks() ([]*Task, error) {
	// id 는 AUTOINCREMENT 이므로 같은 시각에 만든 작업도 안정적이고 고유한 순서를 가진다.
	rows, err := d.Query(`SELECT ` + taskCols + ` FROM tasks WHERE deleted_at IS NULL
ORDER BY (pinned_at IS NOT NULL) DESC, pinned_at DESC NULLS LAST, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := d.hydrateTasksContext(out); err != nil {
		return nil, err
	}
	return out, nil
}

// TaskPatch updates task list metadata without changing execution state.
type TaskPatch struct {
	Name   *string
	Remark *string
	Pinned *bool
}

// UpdateTask applies a partial task name/remark/pin mutation and returns the updated row.
// Re-pinning an already pinned task preserves its original position.
func (d *DB) UpdateTask(id int64, patch TaskPatch) (*Task, error) {
	task, err := scanTask(d.QueryRow(`UPDATE tasks SET
	name = CASE WHEN $2 THEN $3 ELSE name END,
	remark = CASE WHEN $5 THEN $6 ELSE remark END,
	pinned_at = CASE
		WHEN $4 IS NULL THEN pinned_at
		WHEN $4 THEN COALESCE(pinned_at, (strftime('%Y-%m-%d %H:%M:%f','now')))
		ELSE NULL
	END
WHERE id=$1 AND deleted_at IS NULL
RETURNING `+taskCols, id, patch.Name != nil, patch.Name, patch.Pinned, patch.Remark != nil, patch.Remark))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return task, nil
}

// GetTask returns one alive task (nil if not found/deleted).
func (d *DB) GetTask(id int64) (*Task, error) {
	t, err := scanTask(d.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=$1 AND deleted_at IS NULL`, id))
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	if err := d.hydrateTaskContext(t); err != nil {
		return nil, err
	}
	return t, nil
}

// SetPaused persists a task's paused flag.
func (d *DB) SetPaused(id int64, paused bool) error {
	_, err := d.Exec(`UPDATE tasks SET paused=$1 WHERE id=$2`, paused, id)
	return err
}

// Enqueue places a task at the tail of the persistent admission queue. Repeating
// the operation while it is already queued keeps its original FIFO position.
func (d *DB) Enqueue(id int64, mode string) error {
	if mode != "bootstrap" && mode != "resume" {
		return fmt.Errorf("invalid queue mode %q", mode)
	}
	_, err := d.Exec(`UPDATE tasks
SET queued=true,
    queued_at=CASE WHEN queued THEN COALESCE(queued_at, next_queue_time($3,(SELECT MAX(queued_at) FROM tasks))) ELSE next_queue_time($3,(SELECT MAX(queued_at) FROM tasks)) END,
    queue_mode=CASE
        WHEN queue_mode='bootstrap' OR $2='bootstrap' THEN 'bootstrap'
        ELSE 'resume'
    END
WHERE id=$1`, id, mode, time.Now())
	return err
}

// Dequeue removes the concurrency hold. clearMode is false when a user pauses a
// queued task, preserving whether its next admission must bootstrap or resume.
func (d *DB) Dequeue(id int64, clearMode bool) error {
	_, err := d.Exec(`UPDATE tasks
SET queued=false,
    queued_at=NULL,
    queue_mode=CASE WHEN $2 THEN '' ELSE queue_mode END
WHERE id=$1`, id, clearMode)
	return err
}

// SetQueued is the compatibility helper used by older callers and tests. New
// scheduling code should use Enqueue/Dequeue so FIFO metadata is explicit.
func (d *DB) SetQueued(id int64, queued bool) error {
	if queued {
		return d.Enqueue(id, "bootstrap")
	}
	return d.Dequeue(id, true)
}

// SetStatus updates a task's lifecycle status. Entering a terminal state
// (done/failed/timeout) stamps completed_at once (COALESCE keeps the first stamp
// stable); moving back to a non-terminal state clears it, so a re-run has no stale
// finish time.
func (d *DB) SetStatus(id int64, status string) error {
	_, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = CASE WHEN $1 IN ('done','failed','timeout') THEN COALESCE(completed_at, (strftime('%Y-%m-%d %H:%M:%f','now'))) ELSE NULL END
 WHERE id = $2`, status, id)
	return err
}

// SetTerminalStatusGuarded sets a terminal status only when the task is NOT already
// terminal, so a completed↔timeout race resolves to the first writer (won=true).
// Returns won=false (no error) when another terminal status already stuck — the
// caller then leaves it alone. Non-terminal transitions / re-run still use SetStatus.
func (d *DB) SetTerminalStatusGuarded(id int64, status string) (won bool, err error) {
	res, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = COALESCE(completed_at, (strftime('%Y-%m-%d %H:%M:%f','now')))
 WHERE id = $2 AND status NOT IN ('done','failed','timeout')`, status, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StampFirstRun records a task's first-real-run moment and computes its absolute
// deadline (= now + timeoutSeconds). Idempotent: only stamps when first_run_at is
// still NULL, so restarts / re-entries keep the original clock. timeoutSeconds<=0
// leaves deadline_at NULL (시간 무제한). Returns the resulting deadline (nil = 무제한/변경 없음).
func (d *DB) StampFirstRun(id int64, timeoutSeconds int) (*time.Time, error) {
	var deadline *time.Time
	err := d.QueryRow(`
UPDATE tasks
   SET first_run_at = COALESCE(first_run_at, (strftime('%Y-%m-%d %H:%M:%f','now'))),
       deadline_at  = CASE
           WHEN first_run_at IS NOT NULL THEN deadline_at            -- 이미 도장이 찍힘: 건드리지 않음
           WHEN $2 > 0 THEN strftime('%Y-%m-%d %H:%M:%f','now','+' || $2 || ' seconds')
           ELSE NULL END
 WHERE id = $1
 RETURNING deadline_at`, id, timeoutSeconds).Scan(&deadline)
	return deadline, err
}

// DeleteTask preserves the historical behavior: remove the task and its
// exploration subgraph while retaining global assets.
func (d *DB) DeleteTask(id int64) error {
	_, err := d.DeleteTaskCascade(id, false, false)
	return err
}

// DeleteTaskCascade hard-deletes a task and its exploration subgraph
// (nodes/edges/activity via ON DELETE CASCADE). Optional standalone findings and
// assets owned only by this task are deleted in the same transaction; shared
// assets are retained and only have this task id detached. deleteLLMRecords is
// optional to preserve callers of the original two-option API.
func (d *DB) DeleteTaskCascade(id int64, deleteAssets, deleteFindings bool, deleteLLMRecords ...bool) (TaskDeleteResult, error) {
	deleteRecords := len(deleteLLMRecords) > 0 && deleteLLMRecords[0]
	return d.DeleteTaskCascadePrepared(id, deleteAssets, deleteFindings, deleteRecords, nil)
}

// DeleteTaskCascadePrepared coordinates reversible external deletion with the
// SQLite cascade. When prepare is non-nil, host ownership is resolved and
// prepare is invoked inside this transaction while asset and anchor writers are
// excluded. The locks remain held through commit, closing the window where a
// host could become shared after its traffic had already been staged.
//
// prepare must only stage reversible work. Any returned error or later database
// error rolls SQLite back; the caller remains responsible for rolling back
// external work that its callback staged successfully.
func (d *DB) DeleteTaskCascadePrepared(
	id int64,
	deleteAssets, deleteFindings, deleteLLMRecords bool,
	prepare func(TaskDeletePreparation) error,
) (TaskDeleteResult, error) {
	var result TaskDeleteResult
	tx, err := d.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var expID int64
	if err := tx.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, id).Scan(&expID); err != nil {
		if err == sql.ErrNoRows {
			return result, nil // already gone
		}
		return result, err
	}

	// SHARE ROW EXCLUSIVE conflicts with every INSERT/UPDATE/DELETE on these
	// tables and with another coordinated deletion. Taking both in one fixed order
	// prevents phantoms (a distinct asset row for the same host) as well as new
	// ownership/anchor references until the deletion transaction commits.
	if deleteAssets || prepare != nil {
	}
	if prepare != nil {
		hosts, err := hostsForTaskDeletion(tx, id, expID)
		if err != nil {
			return result, err
		}
		if err := prepare(TaskDeletePreparation{
			ExplorationID: expID,
			TrafficHosts:  hosts,
		}); err != nil {
			return result, err
		}
	}
	if deleteAssets {
		// Ownership is the union of explicit task_ids and this exploration's anchors.
		// An asset is deletable only when no other live task references it through
		// either mechanism. This covers legacy anchor-only seeds without destroying
		// evidence anchored by another task.
		res, err := tx.Exec(`
WITH candidate_assets AS (
  SELECT id FROM assets WHERE $1 IN (SELECT value FROM json_each(task_ids))
  UNION
  SELECT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes n ON n.id=ea.node_id
  WHERE n.exploration_id=$2
),
deletable AS (
  SELECT a.id
  FROM assets a
  JOIN candidate_assets c ON c.id=a.id
  WHERE NOT EXISTS (
    SELECT 1 FROM tasks t
    WHERE t.id<>$1 AND t.deleted_at IS NULL AND t.id IN (SELECT value FROM json_each(a.task_ids))
  ) AND NOT EXISTS (
    SELECT 1
    FROM exploration_anchors ea
    JOIN exploration_nodes n ON n.id=ea.node_id
    JOIN tasks t ON t.exploration_id=n.exploration_id
    WHERE ea.asset_id=a.id AND t.id<>$1 AND t.deleted_at IS NULL
  )
)
DELETE FROM assets WHERE id IN (SELECT id FROM deletable)`, id, expID)
		if err != nil {
			return result, err
		}
		result.AssetsDeleted, _ = res.RowsAffected()

		res, err = tx.Exec(`UPDATE assets SET task_ids = collection_remove(task_ids, $1) WHERE $1 IN (SELECT value FROM json_each(task_ids))`, id)
		if err != nil {
			return result, err
		}
		result.AssetsDetached, _ = res.RowsAffected()
	}
	if deleteFindings {
		res, err := tx.Exec(`DELETE FROM findings WHERE task_id = $1`, id)
		if err != nil {
			return result, err
		}
		result.FindingsDeleted, _ = res.RowsAffected()
	}
	if deleteLLMRecords {
		res, err := tx.Exec(`DELETE FROM llm_records WHERE COALESCE(task_id,'')=$1`, strconv.FormatInt(id, 10))
		if err != nil {
			return result, err
		}
		result.LLMRecordsDeleted, _ = res.RowsAffected()
	}
	// llm_usage (the token metering ledger) is intentionally NOT deleted with the
	// task — it is kept as historical accounting even after the task is gone.
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id=$1`, id); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
