package app

import (
	"context"
	"log"
	"time"
)

// Scheduler 周期性执行房东超时验房的两段巡检:
//  1. 首次期限巡检: 到期仍未验房 -> 二次提醒并标记逾期;
//  2. 最终期限巡检: 门禁快照 / 状态突变暂停 / 自动全额释放。
//
// 两个巡检都以合同粒度排他事务执行, 多副本部署也安全(行锁 FOR UPDATE + 状态机)。
type Scheduler struct {
	svc      *Service
	interval time.Duration
}

func NewScheduler(svc *Service, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Scheduler{svc: svc, interval: interval}
}

func (sc *Scheduler) Run(ctx context.Context) {
	log.Printf("[scheduler] overdue-inspection scheduler started (interval=%s)", sc.interval)
	ticker := time.NewTicker(sc.interval)
	defer ticker.Stop()
	// 启动后稍等片刻让其他组件就绪, 再执行首轮。
	sc.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Println("[scheduler] stopped")
			return
		case <-ticker.C:
			sc.tick(ctx)
		}
	}
}

func (sc *Scheduler) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[scheduler] panic recovered: %v", r)
		}
	}()
	reminded, err := sc.svc.RunOverdueReminderScan(ctx)
	if err != nil {
		log.Printf("[scheduler] reminder scan error: %v", err)
		return
	}
	if len(reminded) > 0 {
		log.Printf("[scheduler] overdue second reminder sent for %d contract(s): %v", len(reminded), reminded)
	}
	rep, err := sc.svc.RunFinalReleaseScan(ctx)
	if err != nil {
		log.Printf("[scheduler] final release scan error: %v", err)
		return
	}
	if len(rep.SnapshotsTaken) > 0 || len(rep.AutoReleased) > 0 || len(rep.ManualReview) > 0 {
		log.Printf("[scheduler] final scan: snapshots=%d released=%v manual_review=%v reasons=%v",
			len(rep.SnapshotsTaken), rep.AutoReleased, rep.ManualReview, rep.BlockedReasons)
	}
}
