package mem

import (
	"context"
	"sort"
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

const dateFmt = "2006-01-02"

func addDays(date string, n int) string {
	t, err := time.Parse(dateFmt, date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, n).Format(dateFmt)
}

func daysBetween(from, to string) int {
	a, err1 := time.Parse(dateFmt, from)
	b, err2 := time.Parse(dateFmt, to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// dirCharge is one charge as the vendor directory sees it: the Postgres directory_charges row.
type dirCharge struct {
	org         ids.ID
	c           model.Charge
	outstanding int64
}

func (c dirCharge) derivedStatus() string {
	if c.c.Status == model.ChargeOpen && c.outstanding <= c.c.TolerancePaise {
		return model.ChargePaid
	}
	return c.c.Status
}

// ledger derives the directory rows from the ledger state, as the Postgres writer does in each transaction.
func (d dir) ledger() (subs map[ids.ID]model.Subscription, nextStart map[ids.ID]string, charges []dirCharge, st *state) {
	st = d.snapshot()
	subs, nextStart = map[ids.ID]model.Subscription{}, map[ids.ID]string{}
	for t, rows := range st.com.subs {
		for _, s := range rows {
			if s.SupersededAt == nil {
				subs[t] = s
			}
		}
	}
	net := map[ids.ID]map[ids.ID]int64{}
	for t, p := range st.com.pays {
		net[t] = netPaid(p)
	}
	last := map[ids.ID]string{}
	for k, c := range st.com.charges {
		out := int64(0)
		if c.Status == model.ChargeOpen {
			out = max(c.TotalP-net[k.t][c.ID], 0)
		}
		charges = append(charges, dirCharge{org: k.t, c: c, outstanding: out})
		if c.Kind == model.ChargePeriod && c.Status != model.ChargeCancelled && c.PeriodEnd > last[k.t] {
			last[k.t] = c.PeriodEnd
		}
	}
	for t, s := range subs {
		next := s.StartDate
		if l := last[t]; l != "" && addDays(l, 1) > next {
			next = addDays(l, 1)
		}
		nextStart[t] = next
	}
	return
}

func addonQty(s model.Subscription, name string) int {
	n := 0
	for _, a := range s.Addons {
		if a.Name == name {
			n += a.Quantity
		}
	}
	return n
}

func addonsPaise(s model.Subscription) int64 {
	var n int64
	for _, a := range s.Addons {
		n += int64(a.Quantity) * a.AgreedPaise
	}
	return n
}

type sortRow struct {
	row  model.LedgerRow
	sort string
}

func (d dir) LedgerList(_ context.Context, kind, today, afterDue string, after ids.ID, limit int) ([]model.LedgerRow, error) {
	subs, next, charges, st := d.ledger()
	name := func(id ids.ID) string { return st.orgs[id].Name }
	outletCount := func(t ids.ID) int {
		n := 0
		for k := range st.outlets {
			if k.t == t {
				n++
			}
		}
		return n
	}
	version := func(s model.Subscription) (model.PlanVersion, bool) {
		for _, v := range st.com.plans[s.PlanKey].Versions {
			if v.Version == s.PlanVersion {
				return v, true
			}
		}
		return model.PlanVersion{}, false
	}
	live := func(s model.Subscription) bool {
		return s.State != model.SubCancelled && !(s.CancelAtPeriodEnd && s.PaidThrough < today)
	}
	var rows []sortRow
	switch kind {
	case "overdue_charges":
		for _, c := range charges {
			if c.c.Status == model.ChargeOpen && c.outstanding > c.c.TolerancePaise && c.c.DueDate < today {
				age, amt := daysBetween(c.c.DueDate, today), c.outstanding
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: c.org, OrganisationName: name(c.org), SubjectID: c.c.ID, DueDate: c.c.DueDate, AgeDays: &age, AmountP: &amt}, c.c.DueDate})
			}
		}
	case "charges_to_raise":
		for t, s := range subs {
			if s.State == model.SubActive && s.Cycle != model.CycleNone && !(s.CancelAtPeriodEnd && next[t] > s.PaidThrough) && next[t] <= addDays(today, 7) {
				amt := s.AgreedPaise + addonsPaise(s)
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: t, OrganisationName: name(t), SubjectID: s.ID, DueDate: next[t], AmountP: &amt}, next[t]})
			}
		}
	case "charges_without_invoice_reference":
		for _, c := range charges {
			if c.c.Status == model.ChargeOpen && c.c.InvoiceNumber == "" {
				amt := c.c.TotalP
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: c.org, OrganisationName: name(c.org), SubjectID: c.c.ID, DueDate: c.c.DueDate, AmountP: &amt}, c.c.DueDate})
			}
		}
	case "pilots_ending":
		for t, s := range subs {
			if s.State == model.SubPilot && !s.CancelAtPeriodEnd && s.PaidThrough <= addDays(today, 14) {
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: t, OrganisationName: name(t), SubjectID: s.ID, DueDate: s.PaidThrough}, s.PaidThrough})
			}
		}
	case "retired_version_subscriptions":
		for t, s := range subs {
			if v, ok := version(s); ok && live(s) && v.State == model.VersionRetired {
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: t, OrganisationName: name(t), SubjectID: s.ID}, "0001-01-01"})
			}
		}
	case "outlets_over_limit":
		for t, s := range subs {
			if v, ok := version(s); ok && live(s) && outletCount(t) > v.IncludedOutlets+addonQty(s, "extra_outlet") {
				rows = append(rows, sortRow{model.LedgerRow{OrganisationID: t, OrganisationName: name(t), SubjectID: s.ID}, "0001-01-01"})
			}
		}
	default:
		return nil, apperr.New(apperr.NotFound, "not_found", "")
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].sort != rows[j].sort {
			return rows[i].sort < rows[j].sort
		}
		return rows[i].row.SubjectID < rows[j].row.SubjectID
	})
	afterSort := afterDue
	if afterSort == "" {
		afterSort = "0001-01-01"
	}
	var out []model.LedgerRow
	for _, r := range rows {
		if after != "" && (r.sort < afterSort || (r.sort == afterSort && r.row.SubjectID <= after)) {
			continue
		}
		out = append(out, r.row)
	}
	return cut(out, limit), nil
}

func (d dir) LedgerExport(_ context.Context, from, to string) ([]model.ExportRow, error) {
	_, _, charges, st := d.ledger()
	var out []model.ExportRow
	sort.Slice(charges, func(i, j int) bool {
		if charges[i].c.DueDate != charges[j].c.DueDate {
			return charges[i].c.DueDate < charges[j].c.DueDate
		}
		return charges[i].c.ID < charges[j].c.ID
	})
	for _, c := range charges {
		if (from != "" && c.c.DueDate < from) || (to != "" && c.c.DueDate > to) {
			continue
		}
		out = append(out, model.ExportRow{OrganisationID: c.org, OrganisationName: st.orgs[c.org].Name, PeriodStart: c.c.PeriodStart, PeriodEnd: c.c.PeriodEnd,
			TaxableP: c.c.TaxableP, TotalP: c.c.TotalP, DueDate: c.c.DueDate, InvoiceNumber: c.c.InvoiceNumber, Status: c.derivedStatus(), OutstandingP: c.outstanding})
	}
	return out, nil
}
