package app

import (
	"context"
	"errors"
	"fmt"

	"deposit-escrow/internal/auth"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedUsers 固定演示账号。
type SeedUsers struct {
	TenantID   string
	LandlordID string
	AgentID    string
	PlatformID string
	PropertyID string
}

const (
	SeedTenantPhone   = "13800000001"
	SeedLandlordPhone = "13800000002"
	SeedAgentPhone    = "13800000003"
	SeedPlatformPhone = "13800000004"
)

// Seed 创建演示用户/房源, 并在 Redis 中写入固定 token(README 测试账号)。
func Seed(ctx context.Context, pool *pgxpool.Pool, mgr *auth.Manager) (*SeedUsers, error) {
	u := &SeedUsers{}
	upsertUser := func(phone, name, role string) string {
		var id string
		err := pool.QueryRow(ctx, `
			INSERT INTO users(phone,display_name,role) VALUES($1,$2,$3)
			ON CONFLICT (phone) DO UPDATE SET display_name=EXCLUDED.display_name
			RETURNING id::text`, phone, name, role).Scan(&id)
		if err != nil {
			panic(fmt.Errorf("seed user %s: %w", phone, err))
		}
		return id
	}
	u.TenantID = upsertUser(SeedTenantPhone, "租客·小李", "TENANT")
	u.LandlordID = upsertUser(SeedLandlordPhone, "房东·老王", "LANDLORD")
	u.AgentID = upsertUser(SeedAgentPhone, "中介·张姐", "AGENT")
	u.PlatformID = upsertUser(SeedPlatformPhone, "平台客服", "PLATFORM")

	const addr = "建国路 88 号 SOHO 现代城 3-1207"
	var err error
	err = pool.QueryRow(ctx, `SELECT id::text FROM properties WHERE address=$1 LIMIT 1`, addr).Scan(&u.PropertyID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = pool.QueryRow(ctx, `
			INSERT INTO properties(landlord_id,address,city,rooms)
			VALUES($1,$2,'北京','两室一厅') RETURNING id::text`, u.LandlordID, addr).Scan(&u.PropertyID)
	}
	if err != nil {
		return nil, fmt.Errorf("seed property: %w", err)
	}

	tokens := map[string]struct {
		uid, name, role string
	}{
		"tenant-token":   {u.TenantID, "租客·小李", "TENANT"},
		"landlord-token": {u.LandlordID, "房东·老王", "LANDLORD"},
		"agent-token":    {u.AgentID, "中介·张姐", "AGENT"},
		"platform-token": {u.PlatformID, "平台客服", "PLATFORM"},
	}
	for tok, v := range tokens {
		if err := mgr.PutFixedSession(ctx, tok, v.uid, v.name, v.role); err != nil {
			return nil, err
		}
	}
	return u, nil
}
