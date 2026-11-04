package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevenosloan/tributary-utils/readings"
)

// saveForecast writes a forecast file like record does.
func saveForecast(t *testing.T, dir string, f forecast) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(forecastPath(dir, f.Site, f.ReferenceTime), b); err != nil {
		t.Fatal(err)
	}
}

// fakeUSGS serves 15-minute discharge: 100 cfs at every quarter hour for site
// "11111111", with a second, shorter series of 5 cfs. Site "22222222" fails.
func fakeUSGS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch q.Get("monitoring_location_id") {
		case "USGS-11111111":
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if q.Get("parameter_code") != "00060" {
			t.Errorf("parameter_code = %q", q.Get("parameter_code"))
		}
		parts := strings.Split(q.Get("time"), "/")
		start, err1 := time.Parse(time.RFC3339, parts[0])
		end, err2 := time.Parse(time.RFC3339, parts[1])
		if err1 != nil || err2 != nil {
			t.Errorf("bad time %q", q.Get("time"))
		}
		var feats []string
		prop := func(series string, tm time.Time, v float64) string {
			return fmt.Sprintf(`{"properties":{"time_series_id":%q,"monitoring_location_id":"USGS-11111111","parameter_code":"00060","time":%q,"value":"%g","unit_of_measure":"ft^3/s","approval_status":"Provisional","qualifier":null}}`,
				series, tm.Format(time.RFC3339), v)
		}
		for tm := start.Truncate(15 * time.Minute); !tm.After(end); tm = tm.Add(15 * time.Minute) {
			if tm.Before(start) {
				continue
			}
			feats = append(feats, prop("main", tm, 100))
		}
		feats = append(feats, prop("minor", start, 5))
		fmt.Fprintf(w, `{"features":[%s],"links":[]}`, strings.Join(feats, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReportEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	// Model runs 10% and 20% high at lead 1h and 50% high at lead 2h; 3h is in the future.
	saveForecast(t, dir, forecast{Site: "11111111", Label: "headwater", COMID: 1, Reach: "Beaver Kill",
		Gauge: "Beaver Kill at Cooks Falls, NY", ReferenceTime: ref, Unit: "ft^3/s",
		Points: []forecastPoint{{ref.Add(time.Hour), 110}, {ref.Add(2 * time.Hour), 150}, {ref.Add(3 * time.Hour), 999}}})
	saveForecast(t, dir, forecast{Site: "11111111", Label: "headwater", ReferenceTime: ref.Add(time.Hour), Unit: "ft^3/s",
		Points: []forecastPoint{{ref.Add(2 * time.Hour), 120}}})
	saveForecast(t, dir, forecast{Site: "22222222", Label: "large", Gauge: "Beaver Kill at Cooks Falls", Reach: "Cook Brook",
		ReferenceTime: ref, Unit: "ft^3/s", Points: []forecastPoint{{ref.Add(time.Hour), 1}}})
	os.WriteFile(filepath.Join(dir, "forecasts", "11111111", "junk.json"), []byte("{"), 0o644)

	srv := fakeUSGS(t)
	now := ref.Add(2*time.Hour + 30*time.Minute)
	var warn bytes.Buffer
	rep, err := report(ctx, reportConfig{Dir: dir, USGSURL: srv.URL, Now: func() time.Time { return now }}, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Forecasts != 3 || rep.Pairs != 3 || rep.Unit != "ft^3/s" {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(warn.String(), "junk.json") || !strings.Contains(warn.String(), "22222222") {
		t.Errorf("warnings:\n%s", warn.String())
	}
	// Lead 1: 110 and 120 against 100; lead 2: 150 against 100.
	if len(rep.ByLead) != 2 || rep.ByLead[0].Lead != 1 || rep.ByLead[0].N != 2 || !near(rep.ByLead[0].MedAPE, 15) ||
		rep.ByLead[1].Lead != 2 || rep.ByLead[1].N != 1 || !near(rep.ByLead[1].MedAPE, 50) || !near(rep.ByLead[1].MedBias, 50) {
		t.Errorf("by lead = %+v", rep.ByLead)
	}
	if len(rep.BySite) != 2 {
		t.Fatalf("sites = %+v", rep.BySite)
	}
	a, b := rep.BySite[0], rep.BySite[1]
	if a.Site != "11111111" || a.Forecasts != 2 || a.NameMismatch || a.Overall.N != 3 || a.Error != "" {
		t.Errorf("site a = %+v", a)
	}
	if b.Error == "" || b.Overall.N != 0 || !b.NameMismatch {
		t.Errorf("site b = %+v", b)
	}
	if len(rep.ByLabel) != 2 || rep.ByLabel[0].Label != "headwater" || rep.ByLabel[0].Overall.N != 3 {
		t.Errorf("by label = %+v", rep.ByLabel)
	}

	var out bytes.Buffer
	printReport(&out, rep)
	for _, want := range []string{"By label and lead", "headwater", "1-3h", "11111111", "MISMATCH"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table missing %q:\n%s", want, out.String())
		}
	}
	if b, err := json.Marshal(rep); err != nil || !strings.Contains(string(b), "median_abs_pct_error") {
		t.Errorf("json: %v %s", err, b)
	}

	// The sites file's labels replace the saved ones.
	rep, err = report(ctx, reportConfig{Dir: dir, USGSURL: srv.URL, Now: func() time.Time { return now },
		Labels: []site{{"11111111", "mid"}}}, &warn)
	if err != nil || rep.ByLabel[len(rep.ByLabel)-1].Label != "mid" {
		t.Errorf("relabel: %v %+v", err, rep.ByLabel)
	}
}

func TestReportNothingYet(t *testing.T) {
	dir := t.TempDir()
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	saveForecast(t, dir, forecast{Site: "11111111", ReferenceTime: ref, Unit: "ft^3/s",
		Points: []forecastPoint{{ref.Add(time.Hour), 1}}})
	srv := fakeUSGS(t)
	var out, errb bytes.Buffer
	// With the clock before the forecast, every point is still in the future.
	rep, err := report(ctx, reportConfig{Dir: dir, USGSURL: srv.URL,
		Now: func() time.Time { return ref.Add(-time.Hour) }}, &errb)
	if err != nil || rep.Pairs != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	out.Reset()
	printReport(&out, rep)
	if !strings.Contains(out.String(), "No forecast points have been matched") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestReportNoForecasts(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"report", "-dir", t.TempDir()}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "run record first") {
		t.Errorf("exit %d: %s", code, errb.String())
	}
}

// windowUSGS serves the given readings that fall inside the requested time
// window, for site 11111111; any other site fails.
func windowUSGS(t *testing.T, vals []readings.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("monitoring_location_id") != "USGS-11111111" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		parts := strings.Split(q.Get("time"), "/")
		start, _ := time.Parse(time.RFC3339, parts[0])
		end, _ := time.Parse(time.RFC3339, parts[1])
		var feats []string
		for _, v := range vals {
			if v.Time.Before(start) || v.Time.After(end) {
				continue
			}
			feats = append(feats, fmt.Sprintf(`{"properties":{"time_series_id":%q,"time":%q,"value":"%g","unit_of_measure":"ft^3/s"}}`,
				v.SeriesID, v.Time.Format(time.RFC3339), v.Value))
		}
		fmt.Fprintf(w, `{"features":[%s],"links":[]}`, strings.Join(feats, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReportSeriesSwitch(t *testing.T) {
	dir := t.TempDir()
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	saveForecast(t, dir, forecast{Site: "11111111", ReferenceTime: ref, Unit: "ft^3/s",
		Points: []forecastPoint{{ref.Add(1 * time.Hour), 110}, {ref.Add(3 * time.Hour), 220}}})
	// The old series has more values but stops at 13:30; the new one takes over.
	var vals []readings.Value
	for m := 30; m <= 90; m += 15 {
		vals = append(vals, readings.Value{Time: ref.Add(time.Duration(m) * time.Minute), Value: 100, SeriesID: "old"})
	}
	for m := 150; m <= 210; m += 15 {
		vals = append(vals, readings.Value{Time: ref.Add(time.Duration(m) * time.Minute), Value: 200, SeriesID: "new"})
	}
	vals = append(vals, readings.Value{Time: ref.Add(30 * time.Minute), Value: 1, SeriesID: "new"}) // keep old as the larger
	srv := windowUSGS(t, vals)
	rep, err := report(ctx, reportConfig{Dir: dir, USGSURL: srv.URL,
		Now: func() time.Time { return ref.Add(5 * time.Hour) }}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pairs != 2 {
		t.Fatalf("pairs = %d, want 2 (one from each series): %+v", rep.Pairs, rep.ByLead)
	}
}

func TestReportPadsWindowEnd(t *testing.T) {
	dir := t.TempDir()
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	pt := ref.Add(time.Hour)
	saveForecast(t, dir, forecast{Site: "11111111", ReferenceTime: ref, Unit: "ft^3/s",
		Points: []forecastPoint{{pt, 110}}})
	// The only reading is 10 minutes after the last point.
	srv := windowUSGS(t, []readings.Value{{Time: pt.Add(10 * time.Minute), Value: 100, SeriesID: "s"}})
	rep, err := report(ctx, reportConfig{Dir: dir, USGSURL: srv.URL,
		Now: func() time.Time { return pt.Add(time.Hour) }}, &bytes.Buffer{})
	if err != nil || rep.Pairs != 1 {
		t.Fatalf("%v, pairs %d, want 1", err, rep.Pairs)
	}
}

func TestReportExitStatus(t *testing.T) {
	dir := t.TempDir()
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	now := ref.Add(2 * time.Hour)
	for i := 0; i < 13; i++ { // gauges whose fetch fails
		saveForecast(t, dir, forecast{Site: fmt.Sprintf("2000000%d", i), ReferenceTime: ref, Unit: "ft^3/s",
			Points: []forecastPoint{{ref.Add(time.Hour), 1}}})
	}
	// One gauge with nothing to compare yet: its points are all in the future.
	future := forecast{Site: "11111111", ReferenceTime: ref.Add(3 * time.Hour), Unit: "ft^3/s",
		Points: []forecastPoint{{ref.Add(4 * time.Hour), 1}}}
	saveForecast(t, dir, future)
	srv := windowUSGS(t, nil)
	cfg := reportConfig{Dir: dir, USGSURL: srv.URL, Now: func() time.Time { return now }}
	if _, err := report(ctx, cfg, &bytes.Buffer{}); err == nil {
		t.Error("13 failed fetches and one all-future gauge should be an error")
	}
	// Only the all-future gauge: not an error.
	cfg.Dir = t.TempDir()
	saveForecast(t, cfg.Dir, future)
	if rep, err := report(ctx, cfg, &bytes.Buffer{}); err != nil || rep.Pairs != 0 {
		t.Errorf("all-future only: %v %+v", err, rep)
	}
}

func TestSiteReportKeepsLabel(t *testing.T) {
	ref := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fcs := []forecast{
		{Site: "s", Label: "mid", Gauge: "G", Reach: "R", ReferenceTime: ref},
		{Site: "s", ReferenceTime: ref.Add(time.Hour)}, // later forecast without label, names
	}
	sr := siteReport(fcs, nil)
	if sr.Label != "mid" || sr.Gauge != "G" || sr.Reach != "R" {
		t.Errorf("got %+v", sr)
	}
}

func TestRecordLeavesSitesJSONAlone(t *testing.T) {
	f := newFakeUpstreams(t)
	dir := t.TempDir()
	if err := record(ctx, f.config(dir, "11111111"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sites.json")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := record(ctx, f.config(dir, "11111111"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(old) {
		t.Error("sites.json was rewritten though nothing changed")
	}
}
