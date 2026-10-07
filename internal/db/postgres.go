package db

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	migrationsfs "deposit-escrow"
)

// registerUUIDAsText 让通用查询(Rows.Values -> JSON)把 uuid/uuid[] 解码为字符串而不是 16 字节数组。
func registerUUIDAsText(m *pgtype.Map) {
	uuidText := &pgtype.Type{Name: "uuid", OID: pgtype.UUIDOID, Codec: pgtype.TextCodec{}}
	m.RegisterType(uuidText)
	m.RegisterType(&pgtype.Type{
		Name: "_uuid", OID: pgtype.UUIDArrayOID,
		Codec: &pgtype.ArrayCodec{ElementType: uuidText},
	})
}

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		registerUUIDAsText(c.TypeMap())
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// Migrate 按文件名顺序执行嵌入的 SQL, 已执行的版本记入 schema_migrations。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		return err
	}

	entries, err := migrationsfs.FS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var inserted bool
		err := pool.QueryRow(ctx,
			`INSERT INTO schema_migrations(version) VALUES($1) ON CONFLICT DO NOTHING RETURNING true`, name,
		).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // 该迁移已执行过
		}
		if err != nil {
			return err
		}
		sqlb, err := migrationsfs.FS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sqlb)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
