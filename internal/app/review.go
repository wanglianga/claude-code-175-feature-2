package app

import (
	"context"
	"errors"
	"fmt"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
)

// TenantReviewInspection 租客审阅验房结论。
// agree=true 仅记录审阅(租客仍需逐项接受/申诉); agree=false 要求房东补证复验, 验房回到 REINSPECT。
func (s *Service) TenantReviewInspection(ctx context.Context, contractID string, agree bool, comment string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM"); err != nil {
			return err
		}
		var id, st, cst string
		if err := tx.QueryRow(ctx,
			`SELECT i.id::text, i.status, c.status FROM inspections i
			   JOIN contracts c ON c.id=i.contract_id WHERE i.contract_id=$1`,
			contractID).Scan(&id, &st, &cst); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if st != "SUBMITTED" {
			return fmt.Errorf("%w: inspection is %s", ErrBadState, st)
		}
		newSt := "TENANT_REVIEWED"
		newCst := cst
		if !agree {
			newSt = "REINSPECT"
			newCst = domain.StatusInspecting
		}
		if _, err := tx.Exec(ctx,
			`UPDATE inspections SET status=$2, tenant_reviewed_at=now() WHERE id=$1`, id, newSt); err != nil {
			return err
		}
		if newCst != cst {
			if _, err := tx.Exec(ctx, `UPDATE contracts SET status=$2 WHERE id=$1`,
				contractID, newCst); err != nil {
				return err
			}
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "inspection_reviewed",
			map[string]any{"agree": agree, "comment": comment})
		return s.emit(tx, ctx, contractID, "InspectionReviewed",
			map[string]any{"inspection_id": id, "agree": agree, "new_status": newSt}, actor(ctx).ID)
	})
}
