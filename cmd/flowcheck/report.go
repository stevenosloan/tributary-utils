package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stevenosloan/tributary-utils/internal/httpx"
	"github.com/stevenosloan/tributary-utils/readings"
)

// dischargeCode is the USGS parameter code for discharge.
const dischargeCode = "00060"

// reportConcurrency is how many gauges report fetches history for at once.
const reportConcurrency = 4

type reportConfig struct {
	Dir     string
	Labels  []site // if set, overrides the labels saved with the forecasts
	USGSURL string // defaults to readings.DefaultBaseURL
	APIKey  string
	HTTP    *http.Client
	Now     func() time.Time // defaults to time.Now
}

// SiteReport is the accuracy of one gauge's forecasts.
type SiteReport struct {
	Site  string `json:"site"`
	Label string `json:"label,omitempty"`
	Gauge string `json:"gauge_name"`
	Reach string `json:"reach_name"`
	// NameMismatch is set when the gauge and reach names share no significant
	// word: the gauge may be indexed to the wrong reach.
	NameMismatch bool       `json:"name_mismatch"`
	Forecasts    int        `json:"forecasts"`
	First        time.Time  `json:"first_reference_time"`
	Last         time.Time  `json:"last_reference_time"`
	Error        string     `json:"error,omitempty"` // why no observations were fetched
	Overall      Stat       `json:"overall"`
	ByLead       []LeadStat `json:"by_lead"`
}

// LabelReport is the accuracy of the forecasts of all gauges with one label.
type LabelReport struct {
	Label    string       `json:"label"`
	Sites    int          `json:"sites"`
	Overall  Stat         `json:"overall"`
	ByLead   []LeadStat   `json:"by_lead"`
	ByBucket []BucketStat `json:"by_bucket"`
}

// Report is the result of checking every saved forecast against the gauges.
type Report struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Unit        string        `json:"unit"`
	Forecasts   int           `json:"forecasts"`
	Pairs       int           `json:"pairs"`
	Overall     Stat          `json:"overall"`
	ByLead      []LeadStat    `json:"by_lead"`
	ByLabel     []LabelReport `json:"by_label"`
	BySite      []SiteReport  `json:"by_site"`
}

// loadForecasts reads every saved forecast under dir, sorted by site then
// reference time. Files that can't be read are reported in warnings and
// skipped. labels, if non-empty, replaces the saved labels by site.
func loadForecasts(dir string, labels []site) (fcs []forecast, warnings []string, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "forecasts", "*", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	over := map[string]string{}
	for _, s := range labels {
		over[s.No] = s.Label
	}
	for _, path := range files {
		var f forecast
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &f)
		}
		if err == nil && (f.Site == "" || f.ReferenceTime.IsZero()) {
			err = fmt.Errorf("missing site or reference time")
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", path, err))
			continue
		}
		if l, ok := over[f.Site]; ok {
			f.Label = l
		}
		fcs = append(fcs, f)
	}
	sort.SliceStable(fcs, func(i, j int) bool {
		if fcs[i].Site != fcs[j].Site {
			return fcs[i].Site < fcs[j].Site
		}
		return fcs[i].ReferenceTime.Before(fcs[j].ReferenceTime)
	})
	return fcs, warnings, nil
}

// splitSeries splits vs (sorted by time) into the values of the series with
// the most values, the primary, and the rest. A gauge with several sensors is
// judged against the primary, and the others fill its gaps, e.g. when USGS
// switches the series ID partway through the window.
func splitSeries(vs []readings.Value) (primary, rest []readings.Value) {
	count := map[string]int{}
	best := ""
	for _, v := range vs {
		count[v.SeriesID]++
		if n := count[v.SeriesID]; n > count[best] || (n == count[best] && v.SeriesID < best) {
			best = v.SeriesID
		}
	}
	for _, v := range vs {
		if v.SeriesID == best {
			primary = append(primary, v)
		} else {
			rest = append(rest, v)
		}
	}
	return primary, rest
}

// report loads the saved forecasts and checks them against gauge discharge.
func report(ctx context.Context, cfg reportConfig, warn io.Writer) (*Report, error) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	fcs, warnings, err := loadForecasts(cfg.Dir, cfg.Labels)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(warn, "warning:", w)
	}
	if len(fcs) == 0 {
		return nil, fmt.Errorf("no forecasts under %s; run record first", filepath.Join(cfg.Dir, "forecasts"))
	}
	t := now()
	rc := readings.NewClient(&readings.Options{BaseURL: cfg.USGSURL, HTTPClient: cfg.HTTP, APIKey: cfg.APIKey})

	var siteNos []string
	bySite := map[string][]forecast{}
	for _, f := range fcs {
		if _, ok := bySite[f.Site]; !ok {
			siteNos = append(siteNos, f.Site)
		}
		bySite[f.Site] = append(bySite[f.Site], f)
	}

	type siteData struct {
		pairs   []pair
		hadData bool // the fetch succeeded and returned values
		err     error
	}
	data := make([]siteData, len(siteNos))
	fns := make([]func(context.Context) error, len(siteNos))
	for i, no := range siteNos {
		fns[i] = func(ctx context.Context) error {
			data[i].pairs, data[i].hadData, data[i].err = pairSite(ctx, rc, bySite[no], t)
			return nil
		}
	}
	_ = httpx.Parallel(ctx, reportConcurrency, fns...)

	rep := &Report{GeneratedAt: t.UTC(), Forecasts: len(fcs)}
	var all []pair
	for i, no := range siteNos {
		sr := siteReport(bySite[no], data[i].pairs)
		if data[i].err != nil {
			sr.Error = data[i].err.Error()
			fmt.Fprintf(warn, "warning: %s: %v\n", no, data[i].err)
		}
		rep.BySite = append(rep.BySite, sr)
		all = append(all, data[i].pairs...)
		if rep.Unit == "" {
			rep.Unit = bySite[no][0].Unit
		}
	}
	rep.Pairs = len(all)
	rep.Overall = computeStat(all)
	rep.ByLead = byLead(all)
	rep.ByLabel = labelReports(rep.BySite, all)
	// A site with nothing to compare yet (all points in the future) isn't a
	// success; fail if some fetches failed and none returned data.
	var failed, withData int
	for _, d := range data {
		if d.err != nil {
			failed++
		}
		if d.hadData {
			withData++
		}
	}
	if failed > 0 && withData == 0 {
		return rep, fmt.Errorf("could not fetch gauge data for any site (%d of %d failed)", failed, len(data))
	}
	return rep, nil
}

// pairSite fetches the discharge of a site's gauge over the span of its
// forecasts and pairs it with them.
func pairSite(ctx context.Context, rc *readings.Client, fcs []forecast, now time.Time) (pairs []pair, hadData bool, err error) {
	var first, last time.Time
	for _, f := range fcs {
		for _, p := range f.Points {
			if first.IsZero() || p.Time.Before(first) {
				first = p.Time
			}
			if p.Time.After(last) {
				last = p.Time
			}
		}
	}
	// Pad the start by an hour and the end by matchWindow, so readings just
	// outside the first and last points are fetched, but never ask for the
	// future.
	end := last.Add(matchWindow)
	if end.After(now) {
		end = now
	}
	start := first.Add(-time.Hour)
	if first.IsZero() || !start.Before(end) {
		return nil, false, nil // nothing to compare yet
	}
	vs, err := rc.History(ctx, fcs[0].Site, dischargeCode, start, end)
	if err != nil {
		return nil, false, err
	}
	obs, others := splitSeries(vs)
	if len(obs) > 0 && obs[0].Unit != "" {
		for _, f := range fcs {
			if f.Unit != "" && f.Unit != obs[0].Unit {
				return nil, len(vs) > 0, fmt.Errorf("forecast unit %q differs from gauge unit %q", f.Unit, obs[0].Unit)
			}
		}
	}
	var out []pair
	for _, f := range fcs {
		out = append(out, pairForecast(f, obs, others, now)...)
	}
	return out, len(vs) > 0, nil
}

// siteReport summarizes one gauge from its forecasts (sorted by reference
// time) and pairs. Names come from the latest forecast that has them.
func siteReport(fcs []forecast, pairs []pair) SiteReport {
	sr := SiteReport{Site: fcs[0].Site, Forecasts: len(fcs),
		First: fcs[0].ReferenceTime, Last: fcs[len(fcs)-1].ReferenceTime}
	for _, f := range fcs {
		if f.Label != "" {
			sr.Label = f.Label
		}
		if f.Gauge != "" {
			sr.Gauge = f.Gauge
		}
		if f.Reach != "" {
			sr.Reach = f.Reach
		}
	}
	sr.NameMismatch = namesMismatch(sr.Gauge, sr.Reach)
	sr.Overall = computeStat(pairs)
	sr.ByLead = byLead(pairs)
	return sr
}

// labelReports groups pairs by their site's label, labels in name order. A
// site with no label is grouped as "(none)".
func labelReports(sites []SiteReport, pairs []pair) []LabelReport {
	labelOf := map[string]string{}
	count := map[string]int{}
	for _, s := range sites {
		l := labelName(s.Label)
		labelOf[s.Site] = l
		count[l]++
	}
	groups := map[string][]pair{}
	for _, p := range pairs {
		groups[labelOf[p.Site]] = append(groups[labelOf[p.Site]], p)
	}
	var names []string
	for l := range count {
		names = append(names, l)
	}
	sort.Strings(names)
	var out []LabelReport
	for _, l := range names {
		g := groups[l]
		out = append(out, LabelReport{Label: l, Sites: count[l], Overall: computeStat(g),
			ByLead: byLead(g), ByBucket: byBucket(g)})
	}
	return out
}

func labelName(l string) string {
	if l == "" {
		return "(none)"
	}
	return l
}

func pct(v float64, n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", v)
}

func bias(v float64, n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%+.1f%%", v)
}

func mae(s Stat) string {
	if s.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", s.MAE)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// printReport writes r as readable tables.
func printReport(w io.Writer, r *Report) {
	var first, last time.Time
	for _, s := range r.BySite {
		if first.IsZero() || s.First.Before(first) {
			first = s.First
		}
		if s.Last.After(last) {
			last = s.Last
		}
	}
	fmt.Fprintf(w, "%d forecasts from %d gauges, runs %s to %s; %d forecast/gauge pairs matched (flow in %s)\n",
		r.Forecasts, len(r.BySite), day(first), day(last), r.Pairs, r.Unit)
	if r.Pairs == 0 {
		fmt.Fprintln(w, "\nNo forecast points have been matched with gauge readings yet: the forecasts are")
		fmt.Fprintln(w, "for times that haven't happened, or the gauges returned nothing. Run record")
		fmt.Fprintln(w, "for a while, then run report again.")
	}
	fmt.Fprintln(w, "\nErrors are percent of the gauge value: positive bias means the model is high.")
	fmt.Fprintln(w, "Short leads are flattered at gauged reaches, where the model is nudged toward")
	fmt.Fprintln(w, "the gauge; trust the 12-18h rows more for ungauged reaches.")

	tw := func() *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

	fmt.Fprintln(w, "\nAll gauges, by lead hour")
	t := tw()
	fmt.Fprintln(t, "LEAD\tN\tMED ABS ERR\tMED BIAS\tMAE")
	for _, l := range r.ByLead {
		fmt.Fprintf(t, "%dh\t%d\t%s\t%s\t%s\n", l.Lead, l.N, pct(l.MedAPE, l.NPct), bias(l.MedBias, l.NPct), mae(l.Stat))
	}
	fmt.Fprintf(t, "all\t%d\t%s\t%s\t%s\n", r.Overall.N, pct(r.Overall.MedAPE, r.Overall.NPct),
		bias(r.Overall.MedBias, r.Overall.NPct), mae(r.Overall))
	t.Flush()

	fmt.Fprintln(w, "\nBy label and lead")
	t = tw()
	fmt.Fprintln(t, "LABEL\tGAUGES\tLEAD\tN\tMED ABS ERR\tMED BIAS\tMAE")
	for _, l := range r.ByLabel {
		for _, b := range l.ByBucket {
			fmt.Fprintf(t, "%s\t%d\t%s\t%d\t%s\t%s\t%s\n", l.Label, l.Sites, b.Bucket, b.N,
				pct(b.MedAPE, b.NPct), bias(b.MedBias, b.NPct), mae(b.Stat))
		}
		if len(l.ByBucket) == 0 {
			fmt.Fprintf(t, "%s\t%d\t-\t0\t-\t-\t-\n", l.Label, l.Sites)
		}
	}
	t.Flush()

	fmt.Fprintln(w, "\nBy gauge: median absolute error at some leads")
	t = tw()
	fmt.Fprintln(t, "GAUGE\tLABEL\tFORECASTS\tFROM\tTO\tN\t1h\t6h\t12h\t18h\tALL\tBIAS\tNAMES")
	leads := []int{1, 6, 12, 18}
	for _, s := range r.BySite {
		cells := make([]string, len(leads))
		for i, want := range leads {
			cells[i] = "-"
			for _, l := range s.ByLead {
				if l.Lead == want {
					cells[i] = pct(l.MedAPE, l.NPct)
				}
			}
		}
		var notes []string
		if s.NameMismatch {
			notes = append(notes, "MISMATCH: "+s.Gauge+" / "+s.Reach)
		}
		if s.Error != "" {
			notes = append(notes, "no gauge data")
		}
		flag := "ok"
		if len(notes) > 0 {
			flag = strings.Join(notes, "; ")
		}
		fmt.Fprintf(t, "%s\t%s\t%d\t%s\t%s\t%d\t%s\t%s\t%s\n", s.Site, labelName(s.Label), s.Forecasts,
			day(s.First), day(s.Last), s.Overall.N, strings.Join(cells, "\t"),
			pct(s.Overall.MedAPE, s.Overall.NPct), bias(s.Overall.MedBias, s.Overall.NPct)+"\t"+flag)
	}
	t.Flush()
}
