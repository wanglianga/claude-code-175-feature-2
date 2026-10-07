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
	r.Handle("POST", "api/v1/contracts/{id}/inspection/start", s.withAuth(s.startInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/submit", s.withAuth(s.submitInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/review", s.withAuth(s.reviewInspection))
	// 房东打开验房单(送达/打开留痕) 与提醒查询
	r.Handle("POST", "api/v1/contracts/{id}/inspection/opened", s.withAuth(s.markInspectionOpened))
	r.Handle("GET", "api/v1/contracts/{id}/reminders", s.withAuth(s.listReminders))

	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/accept", s.withAuth(s.acceptItem))
	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/contest", s.withAuth(s.contestItem))

	r.Handle("POST", "api/v1/contracts/{id}/disputes/inspection-delay", s.withAuth(s.openDelayDispute))
	r.Handle("POST", "api/v1/contracts/{id}/disputes/{did}/actions", s.withAuth(s.disputeAction))
	r.Handle("GET", "api/v1/contracts/{id}/disputes", s.withAuth(s.listDisputes))

	r.Handle("POST", "api/v1/contracts/{id}/settlement/propose", s.withAuth(s.proposeSettlement))
	r.Handle("POST", "api/v1/contracts/{id}/settlement/confirm", s.withAuth(s.confirmSettlement))

	// 押金释放后: 房东只能提交超时说明与新证据(不会自动扣款)
	r.Handle("POST", "api/v1/contracts/{id}/post-release-claims", s.withAuth(s.submitPostReleaseClaim))

	// 平台: 逾期未验房巡检(旧) + 超时验房催办/自动释放巡检(新)
	r.Handle("POST", "api/v1/admin/overdue-inspections", s.withAuth(s.autoOverdue))
	r.Handle("POST", "api/v1/admin/overdue-sweep", s.withAuth(s.overdueSweep))
	r.Handle("GET", "api/v1/admin/release-jobs", s.withAuth(s.listReleaseJobs))
	r.Handle("POST", "api/v1/admin/contracts/{id}/release-review", s.withAuth(s.resolveReleaseReview))
	r.Handle("POST", "api/v1/admin/post-release-claims/{cid}/decision", s.withAuth(s.decidePostReleaseClaim))

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

// ---------- 房东超时验房处理 ----------

func (s *Server) markInspectionOpened(w http.ResponseWriter, r *http.Request, a app.Actor) {
	n, err := s.svc.MarkInspectionOpened(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"newly_opened": n})
}

func (s *Server) listReminders(w http.ResponseWriter, r *http.Request, a app.Actor) {
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
	writeJSON(w, 200, map[string]any{
		"items": tl.Reminders,
		"job":   tl.ReleaseJob,
	})
}

func (s *Server) submitPostReleaseClaim(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.PostReleaseClaimInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SubmitPostReleaseClaim(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"claim_id": id,
		"note":     "已登记超时说明; 不会从已释放押金中自动扣款",
	})
}

func (s *Server) overdueSweep(w http.ResponseWriter, r *http.Request, a app.Actor) {
	// 默认完整执行; ?execute=false 只做最终期限评估(任务停在 READY), 用于预检/演示状态变化拦截。
	execute := r.URL.Query().Get("execute") != "false"
	res, err := s.svc.SweepOverdueInspections(app.WithActor(r.Context(), a), execute)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) listReleaseJobs(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if a.Role != "PLATFORM" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "FORBIDDEN", "message": "platform only"})
		return
	}
	items, err := s.q.ListReleaseJobs(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) resolveReleaseReview(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Decision string `json:"decision"` // RELEASE / REOPEN_INSPECTION / KEEP_REVIEW
		Note     string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.ResolveReleaseReview(app.WithActor(r.Context(), a), Param(r, "id"), in.Decision, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) decidePostReleaseClaim(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Decision string `json:"decision"` // UPHELD / REJECTED
		Note     string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.DecidePostReleaseClaim(app.WithActor(r.Context(), a), Param(r, "cid"), in.Decision, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"decision": in.Decision, "auto_deducted": false})
}
