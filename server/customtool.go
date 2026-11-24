package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은 사용자 정의 도구 실행기를 구현한다(docs/自定义工具设计.md). system=false 인 tools 행을
// kind 별로 분배한다: command(명령 렌더링→Bash 하위 run 재사용), script(Python 만. 임시 파일에 쓰고,
// stdin=파라미터 JSON + env TOOL_*, 설정된 인터프리터 사용), http(네이티브 요청+프록시 설정 가능). 이 도구들은
// 트래픽/오케스트레이션 도구처럼 seed 가 필요 없고(원래 tools 테이블에 있음), hostTools 로 주입되며 바인딩에 따라 필터링된다.

// ---------- 사용자 정의 도구 CRUD ----------

type customToolReq struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Kind        string          `json:"kind"` // command | script | http
	Exec        json.RawMessage `json:"exec"`
	Deferred    bool            `json:"deferred"`
}

var reToolKey = reAgentKey // agent key 규칙과 같음: 소문자로 시작 + 소문자/숫자/밑줄

func (s *Server) pgCreateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if !reToolKey.MatchString(req.Key) {
		writeErr(w, 400, "key 는 소문자로 시작하고 소문자/숫자/밑줄만 포함해야 합니다")
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind 는 command / script / http / shell 중 하나여야 합니다")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http 도구는 파라미터 JSON Schema 를 제공해야 합니다(비울 수 없음)")
		return
	}
	if exist, _ := pg.GetTool(req.Key); exist != nil {
		writeErr(w, 409, "이 key 는 이미 존재합니다(내장 또는 사용자 정의 도구)")
		return
	}
	if err := pg.CreateCustomTool(&db.Tool{
		Key: req.Key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"key": req.Key})
}

func (s *Server) pgUpdateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	existing, err := pg.GetTool(key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if existing == nil || existing.System {
		writeErr(w, 400, "사용자 정의 도구만 편집할 수 있습니다")
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind 는 command / script / http / shell 중 하나여야 합니다")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http 도구는 파라미터 JSON Schema 를 제공해야 합니다(비울 수 없음)")
		return
	}
	if err := pg.UpdateCustomTool(&db.Tool{
		Key: key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) pgDeleteCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	if err := pg.DeleteCustomTool(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": key})
}

// testToolReq is a dry-run request from the editor: run the given (possibly unsaved)
// exec spec with sample params, without persisting the tool. Same executor path as a
// real tool call — it runs arbitrary command/script/http on the server, which the
// custom-tool feature already allows, so no new capability is granted.
type testToolReq struct {
	Kind   string          `json:"kind"` // command | script | http
	Exec   json.RawMessage `json:"exec"`
	Params map[string]any  `json:"params"`
}

// pgTestCustomTool executes an exec spec once and returns its raw output + error
// flag, so the editor can debug a tool before saving it. Per-kind timeouts still
// apply from the exec spec (with defaults); the outer ceiling is a hard backstop.
func (s *Server) pgTestCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req testToolReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "유효하지 않은 요청 본문")
		return
	}
	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	tc := &actool.ToolContext{WorkingDir: s.m.dir} // run in the project dir, like a real call
	var res actool.Result
	switch req.Kind {
	case "command":
		res, _ = s.runCommandTool(ctx, req.Exec, params, tc)
	case "script":
		res, _ = s.runScriptTool(ctx, "test", req.Exec, params, tc)
	case "http":
		res, _ = s.runHTTPTool(ctx, req.Exec, params, tc)
	case "shell":
		writeErr(w, 400, "shell 유형 도구는 bash 환경 선언이며 실행할 내용이 없습니다")
		return
	default:
		writeErr(w, 400, "알 수 없는 도구 유형: "+req.Kind)
		return
	}
	writeJSON(w, 200, map[string]any{"output": res.Flatten(), "is_error": res.IsError})
}

// ---------- Python 인터프리터(감지 + DB 저장 + 재정의) ----------

const settingPythonInterp = "python_interpreter"

// detectPython finds a runnable Python 3 interpreter (python3 preferred). PATH
// can contain Windows Store execution aliases which exist but cannot run Python.
func detectPython() string {
	return detectPythonWith(exec.LookPath, probePython3)
}

func detectPythonWith(lookPath func(string) (string, error), probe func(string) bool) string {
	for _, c := range []string{"python3", "python"} {
		if p, err := lookPath(c); err == nil {
			if absolute, err := filepath.Abs(p); err == nil && probe(absolute) {
				return absolute
			}
		}
	}
	return ""
}

func probePython3(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// Bound pipe cleanup as well, in case a broken launcher leaves a child alive.
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.CombinedOutput()
	return err == nil && ctx.Err() == nil && isPython3Version(string(out))
}

func isPython3Version(output string) bool {
	fields := strings.Fields(output)
	return len(fields) == 2 && fields[0] == "Python" && strings.HasPrefix(fields[1], "3.") &&
		len(fields[1]) > 2 && fields[1][2] >= '0' && fields[1][2] <= '9'
}

// Only the two standard per-user Windows Store aliases qualify for automatic
// repair. A broken arbitrary user-configured interpreter must remain untouched.
func isWindowsStorePython(path, platform, localAppData string) bool {
	if platform != "windows" || strings.TrimSpace(localAppData) == "" {
		return false
	}
	path = filepath.Clean(path)
	aliasDir := filepath.Join(localAppData, "Microsoft", "WindowsApps")
	return strings.EqualFold(path, filepath.Join(aliasDir, "python.exe")) ||
		strings.EqualFold(path, filepath.Join(aliasDir, "python3.exe"))
}

func selectPythonInterpreter(stored string, isStoreAlias func(string) bool, probe func(string) bool, detect func() string) (path string, save bool) {
	stored = strings.TrimSpace(stored)
	if stored != "" && (!isStoreAlias(stored) || probe(stored)) {
		return stored, false
	}
	// No valid replacement means no database update, including a failed old alias.
	path = detect()
	return path, path != "" && path != stored
}

func resolvePythonInterpreter(stored string) (string, bool) {
	return selectPythonInterpreter(stored, func(path string) bool {
		return isWindowsStorePython(path, runtime.GOOS, os.Getenv("LOCALAPPDATA"))
	}, probePython3, detectPython)
}

// pythonInterpreter resolves the interpreter: user-set > stored auto-detect > live
// detect. An unusable Windows Store alias may fall back to validated detection.
func (s *Server) pythonInterpreter() string {
	v, _, _ := s.m.pg.GetSetting(settingPythonInterp)
	path, _ := resolvePythonInterpreter(v)
	return path
}

// seedPythonInterpreter fills an unset value, or repairs only a confirmed broken
// Store alias left by old auto-detection. Other configured paths are preserved.
func (s *Server) seedPythonInterpreter() {
	v, _, err := s.m.pg.GetSetting(settingPythonInterp)
	if err != nil {
		return
	}
	if p, save := resolvePythonInterpreter(v); save {
		if err := s.m.pg.SetSetting(settingPythonInterp, p); err != nil {
			log.Printf("[custom-tool] 자동 감지한 python 인터프리터 저장 실패: %v", err)
			return
		}
		if strings.TrimSpace(v) != "" {
			log.Printf("[custom-tool] 실행되지 않는 Windows Store Python 자리표시자 수정: %s -> %s", v, p)
		} else {
			log.Printf("[custom-tool] python 인터프리터를 자동 감지함: %s", p)
		}
	}
}

// ---------- exec 사양 ----------

type commandExec struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}
type scriptExec struct {
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
}
type httpExec struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`
	Body              string            `json:"body"`
	TimeoutMs         int               `json:"timeout_ms"`
	Proxy             string            `json:"proxy"`
	UseRecordingProxy bool              `json:"use_recording_proxy"`
}

func timeoutOr(ms, def int) time.Duration {
	if ms <= 0 {
		return time.Duration(def) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// ---------- 공통 도구 생성 ----------

// customTools builds CoreTools for every user-defined (system=false) tool row.
// shell-kind tools are environment hints only — they surface in the Bash tool
// description via ToolResolve and do NOT create callable tool entries here.
func (s *Server) customTools() ([]actool.CoreTool, error) {
	rows, err := s.m.pg.ListCustomTools()
	if err != nil {
		return nil, err
	}
	out := make([]actool.CoreTool, 0, len(rows))
	for _, t := range rows {
		if t.Kind == "shell" {
			continue // shell hints are handled by ToolResolve → Bash description
		}
		out = append(out, s.buildCustomTool(t))
	}
	return out, nil
}

// buildCustomTool turns one custom-tool row into a CoreTool. Empty schema → a thin
// {args:string} (얇은 껍데기 도구), so command/http templates can use {args}.
func (s *Server) buildCustomTool(t *db.Tool) actool.CoreTool {
	schema := ensureSchema(t.Schema)
	key, kind, execRaw := t.Key, t.Kind, t.Exec
	run := func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
		var params map[string]any
		if len(in) > 0 {
			_ = json.Unmarshal(in, &params)
		}
		if params == nil {
			params = map[string]any{}
		}
		switch kind {
		case "command":
			return s.runCommandTool(ctx, execRaw, params, tc)
		case "script":
			return s.runScriptTool(ctx, key, execRaw, params, tc)
		case "http":
			return s.runHTTPTool(ctx, execRaw, params, tc)
		default:
			return actool.Errorf("알 수 없는 사용자 정의 도구 유형: " + kind), nil
		}
	}
	return actool.Build(actool.Spec{
		Name: key, Description: t.Description, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: run,
	})
}

// hasSchemaProps reports whether raw is a JSON-Schema object with ≥1 property.
// http tools require an explicit schema (the auto {args} shell can't name the
// {param} placeholders in URL/headers/body), so an empty schema is rejected.
func hasSchemaProps(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	props, _ := m["properties"].(map[string]any)
	return len(props) > 0
}

// ensureSchema returns the tool's schema, or a thin {args:string} when none given.
func ensureSchema(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) > 0 {
		return m
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"type": "string", "description": "명령/파라미터(자유 텍스트)"},
		},
	}
}

// ---------- command: 명령 렌더링 → Bash 하위 run 재사용 ----------

func (s *Server) runCommandTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec commandExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Command) == "" {
		return actool.Errorf("command 가 비어 있습니다"), nil
	}
	cmd := renderTemplate(spec.Command, params, shellQuote)
	// Bash 도 쓰는 하위 run 을 재사용한다(Bash CoreTool.Call 을 거침): 안전 floor/시간 초과/
	// 프록시 env/출력 넘침을 자동으로 상속한다. 도구와 Bash 는 같은 층에서 하위를 공유하며, 모델이 Bash 도구를 통해 호출하게 하지 않는다.
	bashIn, _ := json.Marshal(map[string]any{"command": cmd})
	if spec.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 120000))
		defer cancel()
	}
	return actool.NewBash().Call(ctx, bashIn, tc)
}

// ---------- script(Python 만): 임시 파일 + stdin JSON + env ----------

func (s *Server) runScriptTool(ctx context.Context, key string, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec scriptExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Code) == "" {
		return actool.Errorf("script code 가 비어 있습니다"), nil
	}
	interp := s.pythonInterpreter()
	if interp == "" {
		return actool.Errorf("python 인터프리터가 설정되지 않았고 감지되지도 않았습니다(시스템 설정에서 지정하세요)"), nil
	}
	workDir := s.m.dir
	var sessionEnv []string
	if tc != nil {
		if tc.WorkingDir != "" {
			workDir = tc.WorkingDir
		}
		sessionEnv = tc.Env
	}
	body, err := execPython(ctx, interp, key, spec.Code, params, workDir, sessionEnv, timeoutOr(spec.TimeoutMs, 120000))
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(actool.Capture(tc, body)), nil
}

// execPython writes the code to a temp .py under workDir/.tools, runs it via interp
// with the params JSON on stdin + scalar params mirrored to TOOL_<NAME> env, and
// returns combined stdout+stderr (with a timeout/exit note). Standalone + testable.
func execPython(ctx context.Context, interp, key, code string, params map[string]any, workDir string, sessionEnv []string, timeout time.Duration) (string, error) {
	toolsDir := filepath.Join(workDir, ".tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(toolsDir, key+"-*.py")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(code); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, interp, tmp)
	c.Dir = workDir
	c.Env = append(os.Environ(), sessionEnv...) // 대화 프록시 env
	for k, v := range params {                  // 스칼라 파라미터를 TOOL_<NAME> 로 미러링
		if sv, ok := scalarStr(v); ok {
			c.Env = append(c.Env, "TOOL_"+strings.ToUpper(k)+"="+sv)
		}
	}
	pj, _ := json.Marshal(params)
	c.Stdin = bytes.NewReader(pj) // 파라미터 JSON 은 stdin 으로 전달
	out, err := c.CombinedOutput()
	body := string(out)
	if runCtx.Err() == context.DeadlineExceeded {
		body += "\n... [시간 초과로 종료] ..."
	} else if err != nil {
		body += "\n[exit: " + err.Error() + "]"
	}
	return body, nil
}

// ---------- http: 네이티브 요청 + 프록시 ----------

func (s *Server) runHTTPTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec httpExec
	_ = json.Unmarshal(execRaw, &spec)
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = "GET"
	}
	rawURL := renderTemplate(spec.URL, params, identity)
	if strings.TrimSpace(rawURL) == "" {
		return actool.Errorf("http url 이 비어 있습니다"), nil
	}
	var bodyReader io.Reader
	if spec.Body != "" {
		bodyReader = strings.NewReader(renderTemplate(spec.Body, params, identity))
	}
	runCtx, cancel := context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 30000))
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, method, rawURL, bodyReader)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, renderTemplate(v, params, identity))
	}
	client := &http.Client{Timeout: timeoutOr(spec.TimeoutMs, 30000)}
	if tr := s.httpProxyTransport(spec); tr != nil {
		client.Transport = tr
	}
	resp, err := client.Do(req)
	if err != nil {
		return actool.Errorf("요청 실패: " + err.Error()), nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{"status": resp.StatusCode, "body": string(respBody)}
	b, _ := json.Marshal(out)
	return actool.Text(actool.Capture(tc, string(b))), nil
}

// httpProxyTransport builds a Transport for the http tool's proxy config, or nil
// (direct). use_recording_proxy routes through the recording proxy + trusts its CA.
func (s *Server) httpProxyTransport(spec httpExec) *http.Transport {
	proxyStr := strings.TrimSpace(spec.Proxy)
	var caFile string
	if spec.UseRecordingProxy {
		// ProxyAddr is already a complete URL while capture is enabled, and
		// returns the configured global proxy URL when capture is off. Keep both
		// forms intact (including socks5:// and credentials).
		if s.m.TrafficEnabled() {
			if addr := s.m.ProxyAddr(); addr != "" {
				proxyStr = addr
				caFile = s.m.ProxyCACert()
			}
		} else if global := strings.TrimSpace(s.m.GlobalProxy()); global != "" {
			proxyStr = global
		}
	}
	if proxyStr == "" {
		return nil
	}
	pu, err := url.Parse(proxyStr)
	if err != nil {
		return nil
	}
	// Go 1.26+ supports http/https/socks5/socks5h in ProxyURL directly,
	// including proxy userinfo. Keep one code path so existing HTTP tool
	// behavior and proxy authentication remain unchanged.
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return tr
}

// ---------- helpers ----------

func identity(s string) string { return s }

// shellQuote single-quotes a value for safe shell interpolation.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// renderTemplate replaces {name} placeholders with each param's rendered value.
func renderTemplate(tmpl string, params map[string]any, quote func(string) string) string {
	out := tmpl
	for k, v := range params {
		out = strings.ReplaceAll(out, "{"+k+"}", quote(valToStr(v)))
	}
	return out
}

// valToStr renders a param value: scalars as-is, arrays/objects as compact JSON.
func valToStr(v any) string {
	if sv, ok := scalarStr(v); ok {
		return sv
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// scalarStr returns (string, true) for scalar values, ("", false) for arrays/objects.
func scalarStr(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return fmt.Sprintf("%t", x), true
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), true
		}
		return fmt.Sprintf("%g", x), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
