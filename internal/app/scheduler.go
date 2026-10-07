package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunOverdueSweeper 后台定时巡检: 首次期限催办/逾期标记 + 最终期限自动释放或转人工。
// interval<=0 时不启动(测试环境用管理端点手动触发, 保证确定性)。
func RunOverdueSweeper(ctx context.Context, pool *pgxpool.Pool, svc *Service, interval time.Duration) {
	if interval <= 0 {
		log.Println("[boot] overdue sweeper disabled (OVERDUE_SWEEP_SECONDS<=0)")
		return
	}
	platformActor := func() (Actor, bool) {
		var id, name string
		err := pool.QueryRow(ctx,
			`SELECT id::text, display_name FROM users WHERE role='PLATFORM' ORDER BY created_at LIMIT 1`).
			Scan(&id, &name)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("[sweeper] resolve platform user: %v", err)
			}
			return Actor{}, false
		}
		return Actor{ID: id, Role: "PLATFORM", Name: name}, true
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[boot] overdue sweeper started (interval=%s)", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a, ok := platformActor()
			if !ok {
				continue
			}
			res, err := svc.SweepOverdueInspections(WithActor(ctx, a), true)
			if err != nil {
				log.Printf("[sweeper] error: %v", err)
				continue
			}
			if len(res.Overdue)+len(res.AutoReleased)+len(res.Paused)+len(res.Cancelled) > 0 {
				log.Printf("[sweeper] overdue=%v released=%v paused=%v cancelled=%v",
					res.Overdue, res.AutoReleased, res.Paused, res.Cancelled)
			}
		}
	}
}
