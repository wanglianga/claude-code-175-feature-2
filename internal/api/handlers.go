package api

import (
	"net/http"
	"strconv"

	"deposit-escrow/internal/app"
)

// Routes 注册全部 HTTP 路由。
func (s *Server) Routes() *Router {
	r := NewRouter()

	// 文档与健康检查
	r.Handle("GET", "healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	r.Handle("GET", "readyz", s.ready)
	s.mountSwagger(r)

	// 认证
	r.Handle("POST", "api/v1/auth/login", s.login)
	r.Handle("GET", "api/v1/notifications", s.withAuth(s.listNotifications))

	// 用户/房源
	r.Handle("POST", "api/v1/users", s.withAuth(s.createUser))
	r.Handle("POST", "api/v1/properties", s.withAuth(s.createProperty))

	// 合同与租赁全生命周期
	r.Handle("GET", "api/v1/contracts", s.withAuth(s.listContracts))
	r.Handle("POST", "api/v1/contracts", s.withAuth(s.signContract))
	r.Handle("GET", "api/v1/contracts/{id}", s.withAuth(s.getTimeline))
	r.Handle("GET", "api/v1/contracts/{id}/timeline", s.withAuth(s.getTimeline))

	r.Handle("POST", "api/v1/contracts/{id}/deposit", s.withAuth(s.depositIn))
	r.Handle("POST", "api/v1/contracts/{id}/evidence", s.withAuth(s.registerEvidence))
	r.Handle("POST", "api/v1/contracts/{id}/checkin", s.withAuth(s.completeCheckin))

	r.Handle("POST", "api/v1/contracts/{id}/repairs", s.withAuth(s.submitRepair))
	r.Handle("POST", "api/v1/contracts/{id}/repairs/{rid}/decision", s.withAuth(s.decideRepair))

	r.Handle("POST", "api/v1/contracts/{id}/checkout", s.withAuth(s.requestCheckout))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/view", s.withAuth(s.openInspectionSheet))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/start", s.withAuth(s.startInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/submit", s.withAuth(s.submitInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/review", s.withAuth(s.reviewInspection))

	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/accept", s.withAuth(s.acceptItem))
	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/contest", s.withAuth(s.contestItem))

	r.Handle("POST", "api/v1/contracts/{id}/disputes/inspection-delay", s.withAuth(s.openDelayDispute))
	r.Handle("POST", "api/v1/contracts/{id}/disputes/{did}/actions", s.withAuth(s.disputeAction))
	r.Handle("GET", "api/v1/contracts/{id}/disputes", s.withAuth(s.listDisputes))

	r.Handle("POST", "api/v1/contracts/{id}/settlement/propose", s.withAuth(s.proposeSettlement))
	r.Handle("POST", "api/v1/contracts/{id}/settlement/confirm", s.withAuth(s.confirmSettlement))

	// 房东超时验房: 释放后补交超时说明/新证据, 平台复核账外主张
	r.Handle("POST", "api/v1/contracts/{id}/late-claims", s.withAuth(s.submitLateClaim))
	r.Handle("POST", "api/v1/contracts/{id}/late-claims/{clid}/review", s.withAuth(s.reviewLateClaim))

	// 平台: 逾期提醒巡检 / 最终期限自动释放巡检 / 人工复核 / 任务与通知回执
	r.Handle("POST", "api/v1/admin/overdue-inspections", s.withAuth(s.autoOverdue))
	r.Handle("POST", "api/v1/admin/overdue-reminders/scan", s.withAuth(s.scanOverdueReminders))
	r.Handle("POST", "api/v1/admin/final-release/scan", s.withAuth(s.scanFinalRelease))
	r.Handle("POST", "api/v1/contracts/{id}/manual-review", s.withAuth(s.resolveManualReview))
	r.Handle("GET", "api/v1/admin/inspection-tasks", s.withAuth(s.listInspectionTasks))
	r.Handle("POST", "api/v1/admin/notifications/{nid}/delivery", s.withAuth(s.notificationDelivery))

	return r
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := decode(r, &in); err != nil || in.Phone == "" {
		writeJSON(w, 400, map[string]string{"error": "VALIDATION", "message": "phone required"})
		return
	}
	sess, err := s.auth.Login(r.Context(), in.Phone)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"token": sess.Token, "actor": sess.Actor})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CreateUserInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CreateUser(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) createProperty(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CreatePropertyInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CreateProperty(app.WithActor(r.Context(), a), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) listContracts(w http.ResponseWriter, r *http.Request, a app.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rs, err := s.q.ListContracts(r.Context(), a.Role, a.ID, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": rs})
}

func (s *Server) signContract(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SignContractInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SignContract(app.WithActor(r.Context(), a), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id, "status": "SIGNED"})
}

func (s *Server) getTimeline(w http.ResponseWriter, r *http.Request, a app.Actor) {
	id := Param(r, "id")
	if err := s.q.AssertContractParty(r.Context(), id, a); err != nil {
		writeErr(w, err)
		return
	}
	tl, err := s.q.LeaseTimeline(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, tl)
}

func (s *Server) depositIn(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.DepositIn(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "LIVING"})
}

func (s *Server) registerEvidence(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.EvidenceInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.RegisterEvidence(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) completeCheckin(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CheckinInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CompleteCheckin(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"handover_id": id, "phase": "CHECKIN"})
}

func (s *Server) submitRepair(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.RepairInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SubmitRepair(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"repair_id": id, "status": "SUBMITTED"})
}

func (s *Server) decideRepair(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.DecideRepairInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.DecideRepair(app.WithActor(r.Context(), a), Param(r, "id"), Param(r, "rid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": in.Status})
}

func (s *Server) requestCheckout(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CheckoutInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.RequestCheckout(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"handover_id": id})
}

func (s *Server) startInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	id, err := s.svc.StartInspection(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"inspection_id": id, "status": "OPEN"})
}

func (s *Server) submitInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SubmitInspectionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	rep, err := s.svc.SubmitInspection(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"status":           "DEDUCTION_PROPOSED",
		"deductible_total": rep.DeductibleTotal(),
		"facts":            rep.Facts,
		"charges":          rep.Charges,
		"notes":            rep.Notes,
	})
}

func (s *Server) reviewInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Agree   bool   `json:"agree"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.TenantReviewInspection(app.WithActor(r.Context(), a), Param(r, "id"), in.Agree, in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) acceptItem(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.AcceptItem(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "itype"), Param(r, "iid"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ACCEPTED"})
}

func (s *Server) contestItem(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.ContestItemInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.ContestItem(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "itype"), Param(r, "iid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"dispute_id": id, "status": "OPEN"})
}

func (s *Server) openDelayDispute(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Comment string `json:"comment"`
	}
	_ = decode(r, &in)
	id, err := s.svc.OpenInspectionDelayDispute(app.WithActor(r.Context(), a), Param(r, "id"), in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"dispute_id": id, "reason": "INSPECTION_DELAY"})
}

func (s *Server) disputeAction(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.DisputeActionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.DisputeAction(app.WithActor(r.Context(), a), Param(r, "id"), Param(r, "did"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listDisputes(w http.ResponseWriter, r *http.Request, a app.Actor) {
	cid := Param(r, "id")
	if err := s.q.AssertContractParty(r.Context(), cid, a); err != nil {
		writeErr(w, err)
		return
	}
	tl, err := s.q.LeaseTimeline(r.Context(), cid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": tl.Disputes, "actions": tl.Actions})
}

func (s *Server) proposeSettlement(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Note string `json:"note"`
	}
	_ = decode(r, &in)
	id, err := s.svc.ProposeSettlement(app.WithActor(r.Context(), a), Param(r, "id"), in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"settlement_id": id, "status": "PROPOSED"})
}

func (s *Server) confirmSettlement(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.ConfirmSettlement(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) autoOverdue(w http.ResponseWriter, r *http.Request, a app.Actor) {
	ids, err := s.svc.AutoOpenOverdueInspection(app.WithActor(r.Context(), a))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"opened": ids})
}

// ---- 房东超时验房处理 ----

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, a app.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	unread := r.URL.Query().Get("unread") == "true"
	items, err := s.q.ListNotifications(r.Context(), a.ID, unread, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) openInspectionSheet(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.MarkInspectionSheetOpened(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) submitLateClaim(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.LateClaimInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SubmitLateInspectionClaim(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"claim_id": id, "status": "SUBMITTED",
		"note": "账外登记, 不回流托管/不从已释放押金扣款",
	})
}

func (s *Server) reviewLateClaim(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Upheld bool    `json:"upheld"`
		Amount float64 `json:"amount"`
		Note   string  `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.ReviewLateClaim(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "clid"), in.Upheld, in.Amount, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) scanOverdueReminders(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if a.Role != "PLATFORM" {
		writeErr(w, app.ErrForbidden)
		return
	}
	ids, err := s.svc.RunOverdueReminderScan(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"reminded": ids})
}

func (s *Server) scanFinalRelease(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if a.Role != "PLATFORM" {
		writeErr(w, app.ErrForbidden)
		return
	}
	rep, err := s.svc.RunFinalReleaseScan(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) resolveManualReview(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.ResolveManualReviewInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.svc.ResolveManualReview(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) listInspectionTasks(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if a.Role != "PLATFORM" {
		writeErr(w, app.ErrForbidden)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.q.ListInspectionTasks(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) notificationDelivery(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if a.Role != "PLATFORM" {
		writeErr(w, app.ErrForbidden)
		return
	}
	nid, err := strconv.ParseInt(Param(r, "nid"), 10, 64)
	if err != nil {
		writeErr(w, app.ErrValidation)
		return
	}
	var in struct {
		Delivered     bool   `json:"delivered"`
		FailureReason string `json:"failure_reason"`
	}
	_ = decode(r, &in)
	if err := s.svc.MarkNotificationDelivery(r.Context(), nid, in.Delivered, in.FailureReason); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
