// cmd/migrate is the standalone migration-ledger runner. It exists
// separately from cmd/server so a deploy image can run migrations
// before the server image starts (avoids race conditions where two
// replicas both try to migrate at startup).
//
// Usage:
//
//	migrate            # apply pending migrations (default)
//	migrate -status    # print ledger status
//
// Env:
//
//	DATABASE_URL     (required) Postgres connection URL
//	MIGRATIONS_DIR   (optional) directory of *.sql files
//	                 default /migrations (production), ./migrations (dev)
//
// Exit code 0 on success, 1 on any migration failure or misconfiguration.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/migrate"
)

func main() {
	status := flag.Bool("status", false, "print migration ledger status instead of applying")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	dir := os.Getenv("MIGRATIONS_DIR")
	if dir == "" {
		dir = "/migrations"
		if _, err := os.Stat(dir); err != nil {
			// Fall back to ./migrations for local dev where the binary
			// is run from the repo root.
			dir = "./migrations"
		}
	}

	files, err := migrate.LoadFiles(dir)
	if err != nil {
		log.Fatalf("load migrations from %s: %v", dir, err)
	}
	log.Printf("[migrate] %d migration file(s) discovered in %s", len(files), dir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 注册 notice handler —— 迁移文件里的 RAISE WARNING(如 019 的
	// "wechat 未配置"提示)在 lib/pq 默认行为下会被静默丢弃(只在
	// Postgres server log 里,部署日志看不到)。接到 runner 日志(log.Printf,
	// stderr;部署管道通常 stderr/stdout 都捕获)。
	base, err := pq.NewConnector(dsn)
	if err != nil {
		log.Fatalf("connector: %v", err)
	}
	connector := pq.ConnectorWithNoticeHandler(base, func(e *pq.Error) {
		log.Printf("[migrate] db %s: %s", e.Severity, e.Message)
	})
	db := sqlx.NewDb(sql.OpenDB(connector), "postgres")
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()

	if *status {
		if err := migrate.Status(ctx, db, files); err != nil {
			log.Fatalf("status: %v", err)
		}
		return
	}

	applied, skipped, err := migrate.Apply(ctx, db, files)
	if err != nil {
		log.Fatalf("apply: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[migrate] applied=%d skipped=%d\n", applied, skipped)
}
