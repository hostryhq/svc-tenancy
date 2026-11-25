package service

import (
	"testing"
	"time"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

// GST on Hostry's own charges (BR-A1-32): integer paise, CGST and SGST each half the rate rounded half up,
// IGST the full rate rounded half up, nothing when Hostry is not registered, the rate always from configuration.
func TestGST(t *testing.T) {
	cases := []struct {
		name             string
		taxable          int64
		rateBP           int
		registered, same bool
		cgst, sgst, igst int64
	}{
		{"home state splits the rate", 129900, 1800, true, true, 11691, 11691, 0},
		{"another state takes IGST", 129900, 1800, true, false, 0, 0, 23382},
		{"half a paisa rounds up for each half", 1, 1800, true, true, 0, 0, 0},
		{"a rate that leaves a half paisa: 5 paise at 18 percent is 0.45 for each half", 5, 1800, true, true, 0, 0, 0},
		{"50 paise at 18 percent: halves of 4.5 round up to 5", 50, 1800, true, true, 5, 5, 0},
		{"IGST rounds half up", 25, 1800, true, false, 0, 0, 5},
		{"IGST rounds 4.5 up", 50, 900, true, false, 0, 0, 5},
		{"not registered: no tax", 129900, 1800, false, true, 0, 0, 0},
		{"zero rate", 129900, 0, true, true, 0, 0, 0},
		{"nothing to tax", 0, 1800, true, true, 0, 0, 0},
		{"a configured rate other than 18 percent", 100000, 500, true, true, 2500, 2500, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cgst, sgst, igst := GST(c.taxable, c.rateBP, c.registered, c.same)
			if cgst != c.cgst || sgst != c.sgst || igst != c.igst {
				t.Fatalf("GST(%d, %d) = %d %d %d, want %d %d %d", c.taxable, c.rateBP, cgst, sgst, igst, c.cgst, c.sgst, c.igst)
			}
			if cgst != sgst {
				t.Fatal("CGST and SGST are equal")
			}
		})
	}
}

func TestPeriodEnd(t *testing.T) {
	d := func(s string) time.Time { return mustDate(s) }
	for _, c := range []struct{ start, cycle, end string }{
		{"2026-12-01", model.CycleMonthly, "2026-12-31"},
		{"2026-12-15", model.CycleMonthly, "2027-01-14"},
		{"2027-01-31", model.CycleMonthly, "2027-02-27"}, // 31 Jan + 1 month clamps to 28 Feb
		{"2028-01-31", model.CycleMonthly, "2028-02-28"}, // leap year
		{"2026-12-01", model.CycleAnnual, "2027-11-30"},
		{"2028-02-29", model.CycleAnnual, "2029-02-27"},
	} {
		if got := PeriodEnd(d(c.start), c.cycle).Format(dateLayout); got != c.end {
			t.Errorf("%s %s: %s, want %s", c.start, c.cycle, got, c.end)
		}
	}
}

// A charge is paid when payments net of reversals come within the tolerance of the total (BR-A1-41);
// waived and cancelled charges owe nothing; the stored status is never "paid".
func TestChargeView(t *testing.T) {
	c := func(status string) model.Charge {
		return model.Charge{ID: ids.New(), TotalP: 153282, TolerancePaise: 99, Status: status}
	}
	for _, tc := range []struct {
		name        string
		charge      model.Charge
		net         int64
		status      string
		outstanding int64
	}{
		{"unpaid", c(model.ChargeOpen), 0, model.ChargeOpen, 153282},
		{"partly paid", c(model.ChargeOpen), 100000, model.ChargeOpen, 53282},
		{"short by the tolerance", c(model.ChargeOpen), 153183, model.ChargePaid, 99},
		{"short by one paisa more", c(model.ChargeOpen), 153182, model.ChargeOpen, 100},
		{"paid in full", c(model.ChargeOpen), 153282, model.ChargePaid, 0},
		{"a reversal reopens it", c(model.ChargeOpen), 0, model.ChargeOpen, 153282},
		{"waived owes nothing", c(model.ChargeWaived), 0, model.ChargeWaived, 0},
		{"cancelled owes nothing", c(model.ChargeCancelled), 0, model.ChargeCancelled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := chargeView(tc.charge, tc.net)
			if v.Status != tc.status || v.Outstanding != tc.outstanding {
				t.Fatalf("%s %d, want %s %d", v.Status, v.Outstanding, tc.status, tc.outstanding)
			}
		})
	}
}

// A cancellation reads cancelled only from the day after paid_through; nothing else changes the state, so an
// overdue charge can never switch anything off (ADR-0008 S5).
func TestEffectiveState(t *testing.T) {
	sub := model.Subscription{State: model.SubActive, PaidThrough: "2026-12-31"}
	if got := effectiveState(sub, "2027-02-01"); got != model.SubActive {
		t.Fatalf("a subscription that is not cancelled stays active however late it is: %s", got)
	}
	sub.CancelAtPeriodEnd = true
	for today, want := range map[string]string{"2026-12-31": model.SubActive, "2027-01-01": model.SubCancelled} {
		if got := effectiveState(sub, today); got != want {
			t.Errorf("%s: %s, want %s", today, got, want)
		}
	}
	if got := effectiveState(model.Subscription{State: model.SubCancelled}, "2026-01-01"); got != model.SubCancelled {
		t.Fatal(got)
	}
}

func TestTerms(t *testing.T) {
	start := mustDate("2026-12-01")
	if st, paid, err := terms(model.CycleNone, start, ""); err != nil || st != model.SubPilot || paid != "2027-03-01" {
		t.Fatalf("a pilot's review date defaults to 90 days: %s %s %v", st, paid, err)
	}
	if _, paid, _ := terms(model.CycleNone, start, "2026-12-20"); paid != "2026-12-20" {
		t.Fatalf("an explicit review date: %s", paid)
	}
	if _, _, err := terms(model.CycleNone, start, "2026-11-01"); err == nil {
		t.Fatal("a review date before the start is refused")
	}
	if st, paid, _ := terms(model.CycleMonthly, start, ""); st != model.SubActive || paid != "2026-12-31" {
		t.Fatalf("%s %s", st, paid)
	}
}

// "In progress" is open with a business date of today or yesterday in IST (ADR-0014 A1.2 item 3), judged on
// the IST calendar, not UTC: at 20:00 UTC on 24 November it is already 25 November in India.
func TestInProgressWindowIsIST(t *testing.T) {
	s := &Service{clock: clock.Func(func() time.Time { return time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC) })}
	from, to := s.istDates()
	if from != "2026-11-24" || to != "2026-11-25" {
		t.Fatalf("window %s to %s", from, to)
	}
}

func TestSafeCellDefusesFormulas(t *testing.T) {
	for in, want := range map[string]string{"Example Hospitality": "Example Hospitality", "=1+1": "'=1+1", "+91": "'+91", "-x": "'-x", "@SUM(A1)": "'@SUM(A1)", "": ""} {
		if got := safeCell(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
