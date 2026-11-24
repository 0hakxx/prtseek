// Package db provides the embedded SQLite data store.
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/config"
	"modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

//go:embed sqlite_triggers.sql
var triggersSQL string
var schemaMu sync.Mutex

type DB struct{ *sql.DB }

func DSN() (string, string, error) { return config.SQLitePath() }
func IsUniqueViolation(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code() == 2067 || e.Code() == 1555)
}
func IsForeignKeyViolation(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == 787
}

func Open(path string) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("SQLite path is empty")
	}
	if strings.Contains(path, "://") {
		return nil, fmt.Errorf("expected a SQLite file path, not a server DSN")
	}
	name := path
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, err
		}
		uriPath := filepath.ToSlash(abs)
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
		u := url.URL{Scheme: "file", Path: uriPath}
		name = u.String()
	}
	name += "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	raw, err := sql.Open("prtseek-sqlite", name)
	if err != nil {
		return nil, err
	}
	raw.SetMaxOpenConns(8)
	raw.SetMaxIdleConns(8)
	if path == ":memory:" {
		raw.SetMaxOpenConns(1)
	}
	d := &DB{raw}
	schemaMu.Lock()
	defer schemaMu.Unlock()
	fail := func(e error) (*DB, error) { raw.Close(); return nil, e }
	tx, err := raw.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		tx.Rollback()
		return fail(err)
	}
	if version > 1 {
		tx.Rollback()
		return fail(fmt.Errorf("SQLite schema version %d requires a newer application", version))
	}
	if _, err = tx.Exec(schemaSQL + triggersSQL); err != nil {
		tx.Rollback()
		return fail(fmt.Errorf("initialize SQLite schema: %w", err))
	}
	if _, err = tx.Exec(`PRAGMA user_version=1`); err != nil {
		tx.Rollback()
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	if err = d.InitPersonalBackgroundSchema(); err != nil {
		return fail(err)
	}
	if err = d.EnsurePersonalSettings(); err != nil {
		return fail(err)
	}
	if err = d.EnsureLLMRecordsTable(); err != nil {
		return fail(err)
	}
	if err = d.EnsureLLMUsageTable(); err != nil {
		return fail(err)
	}
	if err = d.EnsureConversationTurns(); err != nil {
		return fail(err)
	}
	if err = d.seedBuiltins(); err != nil {
		return fail(err)
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 행을 만들 때의 기본 대화형 shell 스위치. ON CONFLICT 는 사용자가 나중에 수동으로 바꾼 스위치를 덮어쓰지 않음
	runSeconds            *int // 행을 만들 때의 단일 run 벽시계 상한(초). nil=시드 기본값(1200), 0=시간 무제한
}

type promptVar struct{ name, desc, example, source string }

// intp 는 v 의 포인터를 반환하며, builtinAgent 의 선택 필드(예: runSeconds)에 명시적으로 값을 줄 때 쓴다.
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 내장 도구는 DB 에 넣지 않는다. 여기서는 agent + 변수 목록만 seed 한다.
// 참고: planner/worker/mainagent/auto 의 대화형 shell 기본값은 아래 interactive_shell_default_v1
// 블록에서 일괄 true 로 설정한다(이후의 toggle 을 존중함). 여기의 interactiveShell 은 「행을 만들 때부터 기본으로 켜야 하는」 새 agent 에만 준다.
var builtinAgents = []builtinAgent{
	{"goals", "목표 분해", "goals", "침투 작업 목표를 독립적이고 검증 가능한 여러 하위 목표로 분해한다.", []promptVar{
		{"EngagementDescription", "작업 설명(테스트 대상/배경)", "example.com 사이트 테스트", "exploration"},
		// Now 는 전역 runtime 변수이므로(server.globalPromptVars 참고) 더 이상 각 agent 목록에서
		// 중복 정의하지 않는다. 그렇지 않으면 withGlobalVars 가 덧붙일 때 전역 항목과 이름이 충돌한다.
	}, false, nil},
	{"planner", "계획", "planner", "상황을 읽고 목표를 판정하며, 다뤄지지 않은 새 방향이 정말 있을 때만 탐색 의도를 보충한다(작업마다 계획 루프 하나).", []promptVar{
		{"Goal", "작업 전체 목표", "example.com 의 관리자 권한 획득", "exploration"},
		{"AssetSummary", "자산 수/유형 분포 요약(선택)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "메인", "main", "사람-기계 인터페이스: 진행 상황을 관찰하고, 사람의 의도를 hint 나 높은 우선순위 의도로 반영한다.", []promptVar{
		{"Goal", "현재 작업 목표", "example.com 의 관리자 권한 획득", "exploration"},
		{"AssetSummary", "시작 시 상황 요약(선택)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "확인된 취약점 요약(선택)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "실행", "worker", "의도 하나를 받아 실행하고, 발견한 사실/취약점을 지식 그래프에 기록한 뒤 멈춘다.", []promptVar{
		{"ProxyAddr", "기록 프록시 주소(if 이중 문구 제어)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker 자기 식별자(선택)", "worker-1", "runtime"},
	}, false, nil},
	// Auto: 내장 「플랫폼 조작」 agent. 침투 오케스트레이션 루프에 참여하지 않으며, 대화 페이지에서 구동되어 도구로 플랫폼을 조작한다.
	{"auto", "Auto", "assistant", "플랫폼 조작 도우미: 도구로 작업(생성/조회/일시 중지/힌트 주기)과 자산을 관리하며, skill, 사용자 정의 도구, MCP 를 만들거나 수정할 수 있다.", nil, false, nil},
	// 침투 테스트: 내장 「독립 침투」 agent. 대화 페이지에서 구동되며, 혼자서 정찰부터 마무리까지 침투 체인 전체를 진행하고, 스스로 계획하고 실행하고 검증한다. 기본적으로 대화형 shell 이 켜져 있다.
	{"pentest", "침투 테스트", "assistant", "독립 침투 agent: 혼자서 정찰→공격 표면 찾기→깊은 익스플로잇→검증→마무리까지 체인 전체를 진행하며, 스스로 계획하고, 실행하고, 대항적으로 검증한다.", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' 를 각 agent 목록에서 전역 runtime 변수로 올린 뒤, 기존 DB 의 goals 에 'Now' 가 하나 남아 있으면
	// 전역 항목과 이름이 충돌한다(프론트엔드 변수 목록 key 중복). 함께 지운다.
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (사용자가
	// 필요할 때 직접 활성화), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// prtstrike was an optional external integration. Remove its retired setting
	// and catalog entries on every startup so existing workspaces converge with
	// the standalone CTF workflow.
	if _, err := d.Exec(`DELETE FROM settings WHERE key='prtstrike_connection_v1'`); err != nil {
		return fmt.Errorf("remove retired integration setting: %w", err)
	}
	if _, err := d.Exec(`DELETE FROM tools WHERE key IN ('prtstrike_status','prtstrike_ctf_probe')`); err != nil {
		return fmt.Errorf("remove retired integration tools: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw SQLite schema init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[내장] 정부 사이트 (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[내장] 정부 사이트 (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[내장] 교육 사이트 (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[내장] 교육 사이트 (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── 시스템 파괴적 명령 (priority 100) ──────────────────────────────────
		{
			name:     "[내장] 재귀 강제 삭제 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "재귀 강제 삭제(rm -rf / rm --recursive) 실행이 금지되어 있습니다. 시스템이나 대상 환경을 영구적으로 손상시킬 수 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 시스템 핵심 디렉터리 삭제",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "시스템 핵심 경로 삭제가 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 포맷 mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "디스크 포맷(mkfs)이 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 장치 덮어쓰기 dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd 로 디스크 장치를 덮어쓰는 것이 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] Fork 폭탄",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork 폭탄 실행이 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 종료 / 재부팅",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "시스템 종료나 재부팅 명령 실행이 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 모든 프로세스 종료",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 또는 killall -9(모든 프로세스 종료)가 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 지우기 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "디스크 장치에 shred/wipe 지우기를 실행하는 것이 금지되어 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 방화벽 규칙 비우기",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "방화벽 규칙 비우기(iptables -F / nft flush)가 금지되어 있습니다",
			priority: 100,
		},
		// ── 데이터베이스 파괴적 작업 (priority 90) ─────────────────────────────────
		{
			name:     "[내장] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 작업 실행이 금지되어 있습니다. 데이터베이스 객체를 되돌릴 수 없게 파괴할 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE 실행이 금지되어 있습니다. 테이블의 모든 데이터를 비울 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB drop 작업 실행이 금지되어 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis FLUSHALL / FLUSHDB 실행이 금지되어 있습니다. 모든 캐시 데이터를 비울 수 있습니다",
			priority: 90,
		},
		// ── HTTP 파괴적 요청 (priority 80) ──────────────────────────────────
		// Agent 가 DELETE 요청을 보내는 흔한 세 가지 방법:
		//   1. curl -X DELETE / --request DELETE(Bash 도구로 바로 실행하거나 스크립트에 작성)
		//   2. Python HTTP 클라이언트의 .delete() 메서드
		//   3. JS/일반 스크립트의 method: 'DELETE' / method="DELETE"
		{
			name:     "[내장] curl / wget 으로 DELETE 요청 전송",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget 으로 HTTP DELETE 요청을 보내는 것이 금지되어 있습니다. 대상 시스템의 데이터를 삭제할 수 있습니다",
			priority: 80,
		},
		{
			name:     "[내장] Python HTTP 클라이언트 DELETE(requests/httpx/aiohttp)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP 클라이언트로 DELETE 요청을 보내는 것이 금지되어 있습니다",
			priority: 80,
		},
		{
			name:     "[내장] 스크립트에서 HTTP DELETE 메서드 선언(JS/일반)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "스크립트에서 HTTP DELETE 요청을 선언하고 보내는 것이 금지되어 있습니다",
			priority: 80,
		},
		{
			name:     "[내장] 일괄 비우기 / 삭제 인터페이스 경로",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "일괄 비우기나 파기 유형의 인터페이스(/clear /wipe /flush /purge 등) 호출이 금지되어 있습니다",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [내장] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[내장] 파괴적 시스템 명령",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "파괴적 명령이 거부되었습니다(rm -rf / / mkfs / dd / fork bomb / 종료·재부팅 / 디스크 장치 덮어쓰기)",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[내장] 데이터 유출 파이프",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "데이터 유출 파이프로 의심되어 거부되었습니다(명령 출력을 curl/wget/nc 로 외부 전송)",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[내장] 삭제 유형 인터페이스 경로"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"삭제 유형 인터페이스(/delete /remove /unlink /erase 등) 호출이 금지되어 있습니다. 어떤 HTTP 메서드를 쓰든 마찬가지입니다——대부분의 애플리케이션 삭제 인터페이스는 GET/POST 로도 트리거되어 대상 데이터를 실제로 삭제합니다",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
