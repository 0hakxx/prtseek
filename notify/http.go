package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/config"
)

// allowLocalTargets 는 루프백 / 링크 로컬 주소로의 메시지 전달을 허용할지 결정한다.
//
// 기본값은 거부다. 이 대역들은 IM 봇이나 공인 메일 서버가 있을 곳이 아니며, 이 주소들로
// 닿을 수 있는 대상은 민감하다: 같은 기기의 다른 서비스의 관리 포트, 클라우드 환경의 메타데이터 엔드포인트
// (169.254.169.254, 인스턴스 자격 증명을 읽을 수 있음). 전달 주소는 관리자가 설정하지만, XSS/CSRF 에
// 빌려 쓰인 관리 세션이나 같은 JWT 를 쓰는 두 번째 사람은 설정을 바꿔 응답 내용을 되읽을 수 있다
// ——doJSON 은 4xx/5xx 응답 본문의 앞 200 바이트를 last_error 에 쓰고, 전달 기록 인터페이스는
// 그것을 되돌려 보여 주므로, 이것이 반맹목 읽기 수단이 된다.
//
// 하지만 「로컬 SMTP 릴레이」(127.0.0.1:25 의 postfix)는 자체 메일 구축의 흔한 설정이라,
// 일괄적으로 막으면 사람들이 막힌다. 그래서 하드코딩으로 허용하는 대신 명시적인 탈출구를 남긴다:
// prtseek_NOTIFY_ALLOW_LOCAL=1 을 설정하면 허용한다(예전 환경 변수와 호환).
//
// AllowLocalTargetsEnv 로 내보내는 것은 테스트가 명확히 켤 수 있게 하기 위함이다——이 패키지와 server 패키지의
// 테스트는 127.0.0.1 의 httptest 가짜 수신자를 많이 쓰므로, 켜지 않으면 모두 가드에 막힌다.
const AllowLocalTargetsEnv = "prtseek_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(config.Env("NOTIFY_ALLOW_LOCAL"))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP 는 대상 IP 가 「기본적으로 전달을 허용하지 않는」 주소 대역에 속하는지 보고한다.
//
// 루프백, 링크 로컬(클라우드 메타데이터 169.254.169.254 포함), 미지정, 멀티캐스트만 거부한다.
// RFC1918 사설망은 **거부하지 않는다**: 내부망에 자체 구축한 Mattermost / SMTP 릴레이는 매우 흔한 정상 용도이며,
// 이것까지 막으면 실제 환경에서 기능을 아예 쓸 수 없게 된다. 이 절충은 의도적이다——
// 방어는 정말 민감한 대상을 막아야 하지만, 정상적인 배포까지 함께 망가뜨리면 안 된다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)는 IPv4 로 되돌린 뒤 판단해야 한다. 그렇지 않으면 검사를 우회한다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial 은 http.Transport 다이얼러의 Control 훅이며, **연결이 만들어질 때**
// 대상 주소를 검사한다.
//
// 설정 저장 시 검증만 하지 않고 다이얼 단계에 두는 이유: 여기가 최종 적용 지점이기 때문이다.
// 이는 설정 검증을 우회하는 두 가지 경우를 함께 막는다——DNS 리바인딩(검증할 때는 공인 IP 로 해석되고,
// 실제로 연결할 때는 내부망으로 해석됨)과 리디렉션(교차 호스트 이동은 이미 거부하지만, 같은 호스트 이동도
// 경로를 다른 곳으로 가리킬 수 있음).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소 %q 를 해석할 수 없습니다", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬/링크 로컬 주소 %s 로의 전달을 거부합니다(정말 로컬 서비스로 보내야 한다면 %s=1 을 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport 는 기본 Transport 위에 다이얼 가드 하나만 더한다.
// Clone 으로 기본 조정값(연결 풀, HTTP/2, 시간 초과, proxy 등)을 모두 유지해,
// 검사 하나를 더하려고 다른 동작을 바꾸지 않는다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient 는 모든 채널 전달이 공유하는 클라이언트다.
//
// 프로젝트의 전역 출구 프록시(server 측 GlobalProxy)는 일부러 재사용하지 **않는다**: 그 프록시는 침투
// 대상 트래픽용이며 불안정한 터널인 경우가 많은데, 알림의 가용성이 대상 네트워크의 흔들림에 묶여서는 안 된다.
// IM 푸시는 직접 연결이면 된다. 시간 초과는 15초다——그보다 느린 상대는 실제로 이미 장애 상태다.
//
// 교차 호스트 리디렉션을 거부한다: 이 기능의 전달 주소는 모두 「고정된 endpoint 하나」 형태라 정상적으로는
// 다른 호스트로 리디렉션되지 않는다. 그런데 이 서비스들의 자격 증명(딩톡 access_token, 위챗 워크 key, Telegram 의
// bot token)은 **URL 안에 있으므로**, 교차 호스트 이동을 따라가면 자격 증명을 리디렉션 대상에게 넘기는 셈이다. 같은 호스트
// 안의 이동(예: 끝에 슬래시 보충)은 여전히 허용한다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리디렉션 횟수가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("교차 호스트 리디렉션을 거부합니다(%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit 는 응답 본문을 읽는 크기를 제한한다. 상대가 이상하면 아주 큰 내용을 돌려줄 수 있는데, 우리에게 필요한 것은
// 전달 기록에 표시할 오류 코드와 짧은 오류 설명뿐이다.
const respBodyLimit = 8 << 10

// doJSON 은 요청을 한 번 보내고 응답 본문(길이 제한됨)을 반환한다.
//
// payload 가 nil 이면 빈 body 를 보낸다(GET 이나 플랫폼이 body 를 요구하지 않는 경우).
// headers 의 키와 값은 그대로 붙이며, 일반 Webhook 의 사용자 정의 헤더에 쓴다.
//
// 오류 분류가 이 함수의 핵심 역할이다: 네트워크 계층 실패와 5xx/408/429 는 「재시도 가능」,
// 나머지 4xx 는 「영구 실패」로 분류한다——403 을 재시도하는 것은 같은 오류를 로그에 세 번 찍을 뿐이다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 버그(설정 필드 타입 오류)이므로 재시도해도 나아지지 않는다.
			return nil, Permanent(fmt.Errorf("요청 본문 구성 실패: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL 이 잘못됨——대개 사용자가 주소를 잘못 입력한 것이므로 영구 실패에 해당한다.
		// 여기서도 err 를 그대로 전달하면 안 된다: url.Parse 의 오류 텍스트에는 전체 주소가 들어 있다.
		return nil, Permanent(fmt.Errorf("요청 주소가 잘못되었습니다: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부, DNS 실패, 시간 초과——대부분 일시적 장애이므로 백오프 재시도에 맡긴다.
		//
		// 오류 텍스트는 반드시 민감 정보를 제거한 뒤 밖으로 전달해야 한다. 이유: http.Client.Do 가 반환하는 것은 *url.Error 이며,
		// 그 Error() 는 `Op "전체URL": 하위 오류` 인데, 이 기능의 여러 서비스 자격 증명은 **URL 안에 있다**
		// (딩톡 access_token, 위챗 워크 key, 페이슈 hook id, Telegram /bot<token>/).
		// 제거하지 않으면 자격 증명이 이 오류 문자열을 따라 네 곳으로 흘러간다: notification_deliveries
		// 의 last_error(평문 저장), 전달 기록 인터페이스의 응답(**채널 설정의 마스킹을 우회**),
		// 서버 로그, 테스트 전송 인터페이스가 프론트엔드에 돌려주는 502 텍스트.
		return nil, fmt.Errorf("요청 실패: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답 읽기 실패: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429(속도 제한)와 408(시간 초과)은 재시도할 가치가 있다. 나머지 4xx 는 설정이나 권한 문제라 재시도는 의미가 없다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대 쪽 속도 제한 또는 시간 초과 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 쪽 서비스 이상 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대 쪽이 요청을 거부함 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet 은 응답 본문을 한 줄짜리 짧은 텍스트로 압축해 오류 메시지에 쓴다. 응답에는 줄바꿈과 많은 공백이 있을 수 있어,
// last_error 에 그대로 넣으면 전달 기록 페이지의 레이아웃이 깨진다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget 은 전달 주소를 「scheme://host/…」로 압축해 오류 메시지에 쓴다.
//
// 이 패키지의 유일한 주소 민감 정보 제거 기준이며, 일부러 **충분히 거칠게** 만들었다: scheme 과 host 외에는
// 모두 버린다. URL 의 어느 부분이 자격 증명인지 판단하는 「일반적이고 안전한」 방법이 없기 때문이다:
//
//	딩톡   자격 증명이 query 에      /robot/send?access_token=xxx
//	위챗 워크 자격 증명이 query 에      /cgi-bin/webhook/send?key=xxx
//	페이슈   자격 증명이 **경로 끝부분**에 /open-apis/bot/v2/hook/<hook_id>
//	Telegram 자격 증명이 **경로 중간**에 /bot<token>/sendMessage
//
// 「쓸모 있는 부분만 남기려면」 채널별로 패치를 해야 하고, 하나라도 빠뜨리면 그것이 곧 자격 증명 유출이다.
// host 만 남겨도 문제를 찾기에는 충분하며(DNS 해석 불가, 연결 불가, 인증서 오류 모두 위치를 알 수 있음),
// 구체적으로 어느 봇인지는 채널 설정의 마스킹된 끝자리 안내로 알아본다.
//
// 파싱에 실패하면 고정된 자리표시자를 반환한다——원본 문자열은 절대 되돌려 보여 주지 않는다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소를 파싱할 수 없음)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError 는 전송 계층 오류에서 주소를 걷어 내고 하위 원인만 남긴다.
//
// *url.Error 의 구조는 {Op, URL, Err} 이며, Error() 는 URL 까지 함께 출력한다.
// 여기서는 Err 필드를 명시적으로 꺼내 그 Error() 를 피한다——나중에 문자열을 치환하는 것보다 더 확실하다.
// 치환은 URL 인코딩/이스케이프된 여러 변형을 올바르게 다뤄야 해서 빠뜨리기 쉽기 때문이다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error 가 아닌 것(예: 리디렉션 정책이 반환한 오류)에도 주소가 있을 수 있으므로, 모두 민감 정보 제거를 거친다.
	return redactURLsInText(err.Error())
}

// redactURLsInText 는 텍스트에 나타난 http(s) 주소를 민감 정보가 제거된 형태로 바꾼다.
//
// 구조화된 필드를 얻을 수 없는 오류(리디렉션 정책 오류, 서드파티 라이브러리의 사용자 정의 오류)의 대비책이다.
// http/https 접두사만 인식하며, 공백과 따옴표로 나눈다——주소에는 이 두 종류의 문자가 들어가지 않는다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
