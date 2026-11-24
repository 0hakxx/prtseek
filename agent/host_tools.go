package agent

import (
	"runtime"

	actool "github.com/Autumn-27/norma/tool"
)

func shellEnvironment() string {
	if runtime.GOOS == "windows" {
		return "로컬 Windows. Bash 도구는 실제로 powershell -NoProfile -NonInteractive -Command(Windows PowerShell 5.1)를 실행한다. bash heredoc 이나 && 를 쓰지 않는다. 여러 줄 스크립트는 먼저 Write 로 저장한 뒤 python 파일명으로 실행한다. PowerShell 의 curl 별칭을 피하려면 curl.exe 를 쓴다. 텍스트 파일은 UTF-8 로 명시하고, 경로에 공백이 있으면 따옴표로 감싼다. HTTP(S) 프록시와 CA 는 환경 변수로 설정되어 있다. 인증서 오류가 나면 먼저 CA/프록시를 확인하고, 기본적으로 검증을 끄지 않는다."
	}
	return "로컬 시스템: " + runtime.GOOS + ". Bash 는 시스템 shell 을 사용한다. HTTP(S) 프록시와 CA 는 환경 변수로 설정된다."
}

type hostShellTool struct{ actool.CoreTool }

func (t *hostShellTool) Description() string { return shellEnvironment() }
func (t *hostShellTool) Prompt() string      { return shellEnvironment() }
func (t *hostShellTool) AcceptsRawInput() bool {
	v, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && v.AcceptsRawInput()
}
func hostTools(base []actool.CoreTool, proxy, ca string) []actool.CoreTool {
	for i, tool := range base {
		if tool.Name() == "Bash" {
			base[i] = &hostShellTool{tool}
		}
	}
	return append(base, newObservedWebFetch(proxy, ca))
}

// Keep the Chat HTTP route explicit. Inherited proxies/NO_PROXY must not make
// a shell experiment silently take a different route from WebFetch.
func hostHTTPEnv(proxy, ca string) []string {
	env := proxyEnv(proxy, ca)
	if proxy == "" {
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			env = append(env, key+"=")
		}
		if ca != "" {
			for _, key := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS"} {
				env = append(env, key+"="+ca)
			}
		}
	}
	return append(env, "NO_PROXY=", "no_proxy=")
}
