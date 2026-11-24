package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout 은 각각 연결과 SMTP 세션 전체를 제한한다.
// net/smtp 자체에는 시간 초과 메커니즘이 전혀 없어서, 이 두 가지를 두지 않으면 멈춘 상대가
// 전달 goroutine 을 영원히 붙잡아 둔다——dispatcher 는 단일 goroutine 으로 직렬 처리하므로,
// 알림 시스템 전체가 멈추는 것과 같다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel 은 SMTP 메일 전달을 구현한다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 메일에는 플랫폼 속도 제한이 없지만 도배용으로 쓰면 안 되므로 넉넉한 기본값을 준다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹한다. SMTP 호스트, 계정, 수신자는 비밀이 아니며, 마스킹하면 편집만 번거로워진다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port 는 비밀번호를 어느 서버에 넘길지 결정하고, tls 는 암호화 전송 여부를 결정한다. 셋 중 하나라도 바뀌면
// 비밀번호를 다시 지정해야 한다——덕분에 「TLS 끄기」 단계에서도 자격 증명을 명시적으로 함께 보내야 하며, 무심코 바꿀 수 없다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 유효하지 않습니다(1-65535 여야 함)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 최소 하나 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 상대가 지원하면 업그레이드한다. 평문 세션에서는 자격 증명을 보낼 수 없다(아래 auth 설명 참고).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth 는 암호화되지 않은 연결에서 자격 증명 전송을 거부한다(대상이 localhost 인 경우 제외).
			// 이는 **올바른** 보안 동작이므로 우회하면 안 되지만, 이유를 분명히 풀어 줘야 한다——
			// 그렇지 않으면 사용자는 「unencrypted connection」만 보고 어떻게 해야 할지 모른다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송 거부: 연결이 암호화되지 않았습니다. TLS 를 켜거나, 465 포트(암시적 TLS)를 쓰거나, 「TLS 사용」을 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 가 거부됨", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 가 거부됨", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit 실패는 「메일이 이미 서버에 접수됨」이라는 사실에 영향을 주지 않으므로 그 오류는 무시한다.
	_ = client.Quit()
	// 메일에는 길이 잘림이 없으므로(HTML 본문 전체 전송) 배치 전체를 전달된 것으로 친다.
	return len(m.Items), nil
}

// emailDial 은 SMTP 연결을 만든다.
//
// implicitTLS=true 는 465 같은 「연결 즉시 TLS」 방식이고, false 는 25/587 에서 평문으로 연결한 뒤
// STARTTLS 하는 방식이다. 둘을 섞으면 안 된다: 465 포트에 평문 greeting 을 보내면 바로 끊긴다.
//
// 세션 기한은 **연결하는 곳에서** 미리 설정한다(나중에 보충하지 않음). net/smtp 의 Client 는 하위
// 연결을 내보내지 않은 필드에 숨겨 두어 바깥에서 얻을 수 없으므로, 연결을 넘기고 나면 미리 설정한 deadline 으로만
// 막을 수 있다. 이것으로 핸드셰이크 단계의 블로킹도 함께 막는다.
// Control 에 blockInternalDial 을 걸어 HTTP 계열 채널과 같은 가드를 공유한다. 걸지 않으면 SMTP 가
// SSRF 방어 전체의 빈틈이 된다: host 에 169.254.169.254 나 127.0.0.1 을 넣으면 바로 연결되고,
// smtp.NewClient 핸드셰이크가 실패할 때 상대가 돌려준 줄을 오류에 담아 last_error 를 거쳐
// 전달 기록 인터페이스로 되돌려 보여 주므로 반맹목 읽기 수단이 된다. 「연결 거부 vs 시간 초과」의 소요 시간 차이로
// 포트를 탐지할 수도 있다. 다이얼 단계가 최종 적용 지점이며 DNS 리바인딩도 막는다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError 는 SMTP 응답 코드에 따라 어떤 단계의 실패를 「재시도 가능」과 「영구 실패」로 나눈다.
//
// 반드시 구분해야 하는 이유: SMTP 의 4xx 와 5xx 는 의미가 완전히 다르다——
//   - 4xx(450 그레이리스트, 451 로컬 오류, 452 저장 공간 부족)는 **일시적** 거부이며,
//     정석은 나중에 재시도하는 것이다. 특히 그레이리스트는 거의 매번 첫 전달에서 만난다.
//   - 5xx(550 사용자 없음, 553 주소 잘못됨)는 영구 거부이므로 재시도는 의미가 없다.
//
// 모두 영구 실패로 판정하면, 그레이리스트를 켠 메일 서버는 **모든** 푸시를 첫
// 시도 뒤 failed 로 떨어뜨린다——그런데 이런 실패야말로 자동 재시도가 가장 힘을 발휘해야 하는 경우다.
// 응답 코드는 오류 텍스트의 앞 세 자리 숫자에서 가져온다. 코드를 얻지 못하면 재시도 가능으로 처리한다(한 번 더 시도하는 편이
// 파싱하지 못했다고 일시적 장애일 수 있는 것을 사망 판정하는 것보다 낫다).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode 는 SMTP 오류 텍스트에서 앞쪽 세 자리 응답 코드를 가져오며, 얻지 못하면 0 을 반환한다.
// net/smtp 는 오류 코드 필드를 내보내지 않으므로 텍스트에서만 얻을 수 있다. 형식은 「450 4.7.1 ...」.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage 는 완전한 RFC 5322 메일을 조립한다.
//
// 본문을 base64 로 인코딩하는 이유는 두 가지다: 첫째, SMTP 는 한 줄이 1000 바이트를 넘지 않도록 규정하는데 HTML
// 본문(특히 요약 메일)은 아주 긴 줄이 쉽게 생긴다. 둘째, base64 에는 원래 "." 로 시작하는
// 줄이 나오지 않으므로 SMTP 점 이스케이프의 번거로움을 덜 수 있다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 비 ASCII(한국어·중국어 등) 제목은 반드시 RFC 2047 로 인코딩해야 한다. 그렇지 않으면 클라이언트에서 깨져 보인다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 메일에는 길이의 절대 상한이 없으므로 본문을 자르지 않는다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 는 76자마다 줄을 바꾸며, RFC 2045 를 따른다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
