package app

import (
	"testing"

	"deposit-escrow/internal/domain"
)

func TestGateMetricsBlocker(t *testing.T) {
	cases := []struct {
		name string
		m    gateMetrics
		want string
	}{
		{"全干净可自动释放", gateMetrics{ContractStatus: domain.StatusCheckoutPending}, ""},
		{"未闭合争议优先拦截", gateMetrics{OpenDisputes: 1, OpenRepairs: 2, ContractStatus: domain.StatusCheckoutPending}, domain.BlockOpenDisputes},
		{"未关闭报修", gateMetrics{OpenRepairs: 1, ContractStatus: domain.StatusCheckoutPending}, domain.BlockOpenRepairs},
		{"未定论费用项", gateMetrics{UnsettledCharges: 1, ContractStatus: domain.StatusCheckoutPending}, domain.BlockUnsettledBills},
		{"未结水电账单", gateMetrics{UnsettledUtilities: 1, ContractStatus: domain.StatusCheckoutPending}, domain.BlockUnsettledBills},
		{"提前退租未处理", gateMetrics{ContractStatus: domain.StatusEarlyTerminated}, domain.BlockEarlyTermination},
		{"仍有定论预留", gateMetrics{HardHold: 100, ContractStatus: domain.StatusCheckoutPending}, domain.BlockHolds},
		{"仍有争议软冻结", gateMetrics{DisputedHold: 50, ContractStatus: domain.StatusCheckoutPending}, domain.BlockHolds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.m.blocker()
			if got != tc.want {
				t.Fatalf("blocker()=%q want %q", got, tc.want)
			}
			if tc.want == "" && !tc.m.clean() {
				t.Fatalf("metrics should be clean: %+v", tc.m)
			}
			if tc.want != "" && tc.m.clean() {
				t.Fatalf("metrics should be blocked: %+v", tc.m)
			}
		})
	}
}

func TestGateMetricsFingerprintStable(t *testing.T) {
	// 两轮扫描之间门禁指纹必须保持一致才允许自动释放。
	m := gateMetrics{ContractStatus: domain.StatusCheckoutPending}
	a := mustJSON(m)
	b := mustJSON(m)
	if a != b || a != `{"open_disputes":0,"open_repairs":0,"unsettled_charges":0,"unsettled_utility_bills":0,"hard_hold":0,"disputed_hold":0,"contract_status":"CHECKOUT_PENDING"}` {
		t.Fatalf("unstable fingerprint: %s", a)
	}
	m2 := m
	m2.OpenDisputes = 1
	if mustJSON(m2) == a {
		t.Fatalf("fingerprint must change when a dispute opens")
	}
}
