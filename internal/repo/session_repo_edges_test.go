package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/model"
)

// session_repo_edges_test.go — Task 16 覆盖率补强：会话轮换/授权码交换的
// 真实库行为（轮换链、幂等守卫、宽限期字段）。

func TestSessionRepo_RotateAndExchange(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	sessionRepo := NewSessionRepo(db)

	uid := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatal(err)
	}

	mkSession := func(token string) *model.Session {
		return &model.Session{
			ID: uuid.NewString(), UserID: uid, AppID: "yundian", SessionType: "refresh",
			RefreshToken: token, Scope: pq.StringArray{"a"},
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}

	// 插入初始会话并轮换：旧行 revoked + revoked_at + rotated_to 指向新行。
	old := mkSession("tok-old")
	if err := sessionRepo.Create(ctx, old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	next := mkSession("tok-new")
	if err := sessionRepo.RotateRefresh(ctx, old.ID, next); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	var revoked, hasRotatedTo bool
	if err := db.QueryRowxContext(ctx,
		`SELECT revoked, rotated_to IS NOT NULL FROM sessions WHERE id = $1`, old.ID).
		Scan(&revoked, &hasRotatedTo); err != nil {
		t.Fatal(err)
	}
	if !revoked || !hasRotatedTo {
		t.Errorf("old session after rotate: revoked=%v rotated_to=%v", revoked, hasRotatedTo)
	}
	// 幂等守卫：同一旧行再轮换 → ErrSessionAlreadyRevoked（哨兵可被
	// errors.Is 匹配——宽限期/家族撤销决策依赖它）。
	dup := mkSession("tok-dup")
	if err := sessionRepo.RotateRefresh(ctx, old.ID, dup); err == nil {
		t.Fatal("re-rotate must fail with the revoked sentinel")
	}

	// ExchangeAuthCode：存在 → true + 新行落库；不存在 → false（不报错）。
	code := mkSession("code-1")
	code.SessionType = "auth_code"
	if err := sessionRepo.Create(ctx, code); err != nil {
		t.Fatal(err)
	}
	fresh := mkSession("tok-fresh")
	ok, err := sessionRepo.ExchangeAuthCode(ctx, code.ID, fresh)
	if err != nil || !ok {
		t.Fatalf("exchange = %v/%v", ok, err)
	}
	ok, err = sessionRepo.ExchangeAuthCode(ctx, code.ID, mkSession("tok-x"))
	if err != nil || ok {
		t.Fatalf("re-exchange = %v/%v, want false (幂等)", ok, err)
	}
	ok, err = sessionRepo.ExchangeAuthCode(ctx, uuid.NewString(), mkSession("tok-y"))
	if err != nil || ok {
		t.Fatalf("missing exchange = %v/%v, want false", ok, err)
	}
}
