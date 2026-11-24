package agent

import (
	"bytes"
	"sync"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the SQLite agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now, NowTurn string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now, NowTurn string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now, NowTurn string }
type GoalsVars struct{ EngagementDescription, DataDir, Now, NowTurn string }

// stampLayout is the wall-clock format shared by {{.Now}} and {{.NowTurn}}.
const stampLayout = "2006-01-02 15:04:05 MST"

// clockNow is the package clock; tests replace it to pin {{.NowTurn}}.
var clockNow = time.Now

// nowStr is the server-local wall-clock string rendered fresh on every call. It
// backs {{.NowTurn}} only: the system prompt prefix must stay byte-identical across
// turns or every round re-pays the full prompt (no prefix/context cache hit).
func nowStr() string { return clockNow().Format(stampLayout) }

// sessionStamps pins {{.Now}} per conversation. The stamp is taken on the first turn
// that renders a session's prompt and reused afterwards, so the cached prefix holds
// for the whole conversation. Entries are dropped wholesale at the cap: this is a
// cache-key registry, not state worth evicting individually.
var (
	sessionMu     sync.Mutex
	sessionStamps = map[string]string{}
)

const maxSessionStamps = 4096

// sessionNow returns the conversation start stamp for sessionID. An empty session
// (one-shot renders, template preview) gets the live time instead of a stable one.
func sessionNow(sessionID string) string {
	stamp := nowStr()
	if sessionID == "" {
		return stamp
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if known, ok := sessionStamps[sessionID]; ok {
		return known
	}
	if len(sessionStamps) >= maxSessionStamps {
		clear(sessionStamps)
	}
	sessionStamps[sessionID] = stamp
	return stamp
}

// resetSessionStamps clears the registry; tests only.
func resetSessionStamps() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	clear(sessionStamps)
}

// renderSystem returns the rendered system-prompt BODY (구간 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 중간 산출물
// 출력 규칙) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
