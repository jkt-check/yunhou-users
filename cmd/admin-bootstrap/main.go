// cmd/admin-bootstrap is the OFFLINE operator bootstrap command (设计
// §9.2: 首个管理员通过受控离线 bootstrap 建立，拒绝未配置角色的默认放行).
//
// It grants operator roles to an existing user (identified by -user-id or,
// via social_identities.email, by -email). It is idempotent: re-running it
// never duplicates grants (operator_roles has UNIQUE(user_id, role) and the
// insert is ON CONFLICT DO NOTHING). It does not create users — the user
// must already exist (e.g. logged in once via OAuth).
//
// Usage:
//
//	admin-bootstrap -user-id <uuid> -roles admin -reason "founding operator"
//	admin-bootstrap -email ops@example.com -roles admin,operator
//
// Env: DATABASE_URL (or -dsn). Migrations must already be applied.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/management"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("admin-bootstrap: ")

	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "postgres DSN (default: DATABASE_URL)")
	userID := flag.String("user-id", "", "user UUID to grant roles to")
	email := flag.String("email", "", "resolve the user via social_identities.email")
	roles := flag.String("roles", management.RoleAdmin, "comma-separated roles: admin,operator,auditor")
	reason := flag.String("reason", "offline bootstrap", "grant reason (recorded)")
	timeout := flag.Duration("timeout", 15*time.Second, "db operation timeout")
	flag.Parse()

	granted, target, err := run(*dsn, *userID, *email, *roles, *reason, *timeout)
	if err != nil {
		log.Fatal(err)
	}
	for _, role := range granted {
		log.Printf("role %q ensured for user %s", role, target)
	}
	fmt.Printf("user %s roles: %s\n", target, strings.Join(granted, ","))
}

// run executes the bootstrap and returns the (idempotently ensured) roles.
// It is the testable seam behind main.
func run(dsn, userID, email, roles, reason string, timeout time.Duration) ([]string, string, error) {
	if dsn == "" {
		return nil, "", errors.New("DATABASE_URL or -dsn is required")
	}
	if (userID == "") == (email == "") {
		return nil, "", errors.New("exactly one of -user-id or -email is required")
	}
	if reason == "" {
		return nil, "", errors.New("-reason must not be empty")
	}

	roleList := splitComma(roles)
	for _, r := range roleList {
		if !management.ValidRole(r) {
			return nil, "", fmt.Errorf("unknown role %q (want admin, operator or auditor)", r)
		}
	}

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("connect: %w", err)
	}
	defer db.Close()
	store := inferencepostgres.NewStore(db)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	target := userID
	if email != "" {
		target, err = store.FindUserIDByEmail(ctx, email)
		if err != nil {
			return nil, "", fmt.Errorf("resolve email %q to a user: %w (has the user logged in once?)", email, err)
		}
	}

	// Fail fast when the target user does not exist — a silent no-op grant
	// to a typo'd UUID would leave the deployment with no operator at all.
	var exists bool
	if err := db.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, target); err != nil {
		return nil, "", fmt.Errorf("check user: %w", err)
	}
	if !exists {
		return nil, "", fmt.Errorf("user %s does not exist", target)
	}

	ensured := make([]string, 0, len(roleList))
	for _, role := range roleList {
		if _, err := store.GrantRole(ctx, target, role, nil, reason); err != nil {
			return nil, "", fmt.Errorf("grant %s: %w", role, err)
		}
		ensured = append(ensured, role)
	}
	return ensured, target, nil
}

func splitComma(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
