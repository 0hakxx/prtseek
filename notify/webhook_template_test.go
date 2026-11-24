package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 범용 Webhook 템플릿의 **능력 경계**를 못 박는다.
//
// 여기가 패키지 전체에서 유일하게 「사용자가 제공한 문자열이 코드로 평가되는」 곳이므로, 무엇을 할 수 있고
// 무엇을 할 수 없는지 분명히 하고 그 성질을 테스트로 고정한다——그러지 않으면 향후 누군가 무심코 템플릿 컨텍스트에
// 메서드를 추가하거나 FuncMap 에 readFile 을 넣어 능력 면이 조용히 커지는데, diff 로는
// 무해한 작은 함수 하나로만 보인다.

// TestTemplateContextHasNoMethods 는 가장 중요한 항목이다.
//
// text/template 은 내보낸 메서드를 호출한다({{.Foo}} 는 필드도 메서드도 가져올 수 있음). 그래서 템플릿 컨텍스트가
// 내보낸 메서드를 가진 **어떤** 타입에라도 닿을 수 있으면, 그 메서드를 템플릿 작성자에게 노출하는 셈이다.
// 이 기능의 컨텍스트는 일부러 전부 순수 데이터다(내보낸 필드만 있고 메서드는 없음).
//
// 이것이 실패하면: 누군가 webhookTemplateData / webhookItem 에 메서드를 추가했다는 뜻이다.
// 통과시키기로 결정하기 전에, 그 메서드가 노출하고 싶지 않은 것을 템플릿이 읽는 데 쓰일 수 있는지 먼저 명확히 하라.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s 가 메서드 %d 개(%s)를 노출함: text/template 이 그것들을 호출할 수 있어, "+
				"이 메서드들의 능력을 템플릿 작성자에게 개방하는 셈이다", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal 은 템플릿에 노출되는 함수 집합을 못 박는다.
//
// FuncMap 에 함수가 하나 늘 때마다 능력이 하나 는다. 현재는 json / jsons 뿐이며, 값을 JSON 조각으로
// 직렬화하는 역할이다——파일을 읽지도, 요청을 보내지도, 명령을 실행하지도 못한다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합이 바뀜: %v 얻음, 기대 %v. 함수를 추가하기 전에 그것이 능력 면을 넓히지 않는지 확인하라"+
			"(파일을 읽거나 쓸 수 없고, 네트워크 요청을 보낼 수 없고, 명령을 실행할 수 없음)", got, want)
	}
}

// TestTemplateCannotReachUnknownData 는 템플릿 안의 범위 밖 접근을 다룬다:
// 존재하지 않는 것에 접근하면 뭔가를 에코하지 말고 반드시 실패해야 하며, 실패 메시지가 내부 데이터를 내보내면 안 된다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("존재하지 않는 필드에 접근하면 오류를 내야 함")
	}
	// 오류에 템플릿 컨텍스트의 실제 내용(취약점 제목/요약)이 나타나면 안 된다.
	for _, leak := range []string{"SQL 주입", "파라미터 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 오류가 메시지 내용 %q 를 유출함: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: 템플릿 오타는 설정 오류라 재시도해도 스스로 복구되지 않는다.
// 재시도 가능으로 판정되면, 잘못된 템플릿 하나가 매 전달마다 백오프 세 번을 헛되이 돌린다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 문법 오류는 저장할 때 바로 차단되어야 함")
	}
	// 검증을 우회해 바로 전달하더라도, 반복 재시도가 아니라 반드시 영구 실패로 판정해야 한다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("잘못된 템플릿은 영구 실패로 판정해야 하는데 %v 얻음", err)
	}
}

// TestTemplateCanOnlyProduceJSON 은 「템플릿 렌더링 결과는 반드시 올바른 JSON 이어야 한다」 제약을 다룬다.
// 이것은 「템플릿으로 순수 텍스트를 생성해 다른 프로토콜을 유발」하는 류의 사용도 덤으로 막는다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 올바른 템플릿은 통과한다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("올바른 템플릿은 검증을 통과해야 함: %v", err)
	}
	// JSON 이 아닌 것이 렌더링되면 반드시 거부해야 한다(그대로 내보내지 말고).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON 이 아닌 렌더링은 영구 실패로 판정해야 하는데 %v 얻음", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("오류 메시지는 JSON 문제임을 설명해야 하는데 %v 얻음", err)
	}
}
