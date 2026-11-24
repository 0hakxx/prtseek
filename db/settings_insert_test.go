package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent 는 auth.password_hash 의 대비책이다: 호출자의 GetSetting 검사는
// 데이터베이스 오류로 무효가 될 수 있고, 동시 요청이 끼어들 수도 있어(bcrypt 는 수십 밀리초가 걸림), "최초에만
// 설정 가능"의 보장은 상위의 if 판단이 아니라 기본 키 제약에 있어야 한다.
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("SQLite unavailable (%v)", err)
	}
	defer d.Close()

	// 테스트 전용 key 를 써서 개발 DB 의 실제 auth.password_hash 를 절대 건드리지 않는다.
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("최초 쓰기는 inserted=true 를 반환해야 함")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("키가 이미 있으면 inserted=false 를 반환해야 함")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("값이 %q 로 덮어써짐, %q 를 유지해야 함", got, "first")
	}
}
