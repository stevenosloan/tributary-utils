package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// fakeUpstreams serves NLDI and NWPS for two gauges: 111 (comid 1001) and 222
// (comid 1002). Gauge 333 is unknown to NLDI; gauge 444 (comid 1004) has no
// forecast. nldiHits counts NLDI requests.
type fakeUpstreams struct {
	nldi, nwps *httptest.Server
	mu         sync.Mutex
	nldiHits   int
	ref        string // reference time of the forecast served
}

func newFakeUpstreams(t *testing.T) *fakeUpstreams {
	t.Helper()
	f := &fakeUpstreams{ref: "2026-11-03T12:00:00Z"}
	comids := map[string]int{"11111111": 1001, "22222222": 1002, "44444444": 1004}
	f.nldi = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nldiHits++
		f.mu.Unlock()
		no := strings.TrimPrefix(r.URL.Path, "/nwissite/USGS-")
		id, ok := comids[no]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"features":[{"properties":{"comid":%d,"name":"Gauge %s"}}]}`, id, no)
	}))
	f.nwps = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int
		if _, err := fmt.Sscanf(r.URL.Path, "/reaches/%d/streamflow", &id); err != nil || id == 1004 {
			http.Error(w, `{"code":14,"message":"Data unavailable"}`, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		ref := f.ref
		f.mu.Unlock()
		fmt.Fprintf(w, `{"reach":{"reachId":"%d","name":"Reach %d"},"shortRange":{"series":{"referenceTime":%q,"units":"ft³/s","data":[`+
			`{"validTime":"2026-11-03T13:00:00Z","flow":10.5},{"validTime":"2026-11-03T14:00:00Z","flow":11}]}}}`, id, id, ref)
	}))
	t.Cleanup(func() { f.nldi.Close(); f.nwps.Close() })
	return f
}

func (f *fakeUpstreams) config(dir string, nos ...string) recordConfig {
	cfg := recordConfig{Dir: dir, NLDIURL: f.nldi.URL, NWPSURL: f.nwps.URL}
	for _, n := range nos {
		cfg.Sites = append(cfg.Sites, site{No: n, Label: "lbl"})
	}
	return cfg
}

func TestRecord(t *testing.T) {
	f := newFakeUpstreams(t)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := record(ctx, f.config(dir, "11111111", "22222222"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "saved") != 2 {
		t.Errorf("output:\n%s", out.String())
	}
	path := filepath.Join(dir, "forecasts", "11111111", "20261103T1200Z.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got forecast
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Site != "11111111" || got.Label != "lbl" || got.COMID != 1001 || got.Reach != "Reach 1001" ||
		got.Gauge != "Gauge 11111111" || got.Unit != "ft^3/s" || len(got.Points) != 2 ||
		got.Points[1].Flow != 11 || !got.ReferenceTime.Equal(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("saved %+v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("%d files in site dir, want 1 (no temp files left)", len(entries))
	}

	// Same reference time again: skipped, and NLDI isn't asked again.
	hits := f.nldiHits
	out.Reset()
	if err := record(ctx, f.config(dir, "11111111", "22222222"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "already had") != 2 || strings.Contains(out.String(), "saved") {
		t.Errorf("second run output:\n%s", out.String())
	}
	if f.nldiHits != hits {
		t.Errorf("NLDI asked %d more times despite sites.json", f.nldiHits-hits)
	}

	// A new run is saved alongside.
	f.mu.Lock()
	f.ref = "2026-11-03T13:00:00Z"
	f.mu.Unlock()
	out.Reset()
	if err := record(ctx, f.config(dir, "11111111"), &out); err != nil || !strings.Contains(out.String(), "saved") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "forecasts", "11111111", "20261103T1300Z.json")); err != nil {
		t.Error(err)
	}

	cache, err := loadSiteCache(filepath.Join(dir, "sites.json"))
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := cache.get("22222222"); i.COMID != 1002 || i.Reach != "Reach 1002" || i.Gauge == "" {
		t.Errorf("cache = %+v", i)
	}
}

func TestRecordOneSiteFails(t *testing.T) {
	f := newFakeUpstreams(t)
	dir := t.TempDir()
	var out bytes.Buffer
	// 33333333 is unknown to NLDI, 44444444 has no forecast.
	err := record(ctx, f.config(dir, "33333333", "11111111", "44444444"), &out)
	if err != nil {
		t.Fatalf("one site saved, but got %v", err)
	}
	s := out.String()
	if strings.Count(s, "error:") != 2 || strings.Count(s, "saved") != 1 {
		t.Errorf("output:\n%s", s)
	}
}

func TestRecordAllFail(t *testing.T) {
	f := newFakeUpstreams(t)
	var out bytes.Buffer
	if err := record(ctx, f.config(t.TempDir(), "33333333", "44444444"), &out); err == nil {
		t.Error("want an error when every site fails")
	}
}

func TestRunRecordExitCodes(t *testing.T) {
	f := newFakeUpstreams(t)
	dir := t.TempDir()
	sitesFile := filepath.Join(dir, "sites.txt")
	os.WriteFile(sitesFile, []byte("11111111 a\n33333333 b\n"), 0o644)
	var out, errb bytes.Buffer
	args := []string{"record", "-sites", sitesFile, "-dir", filepath.Join(dir, "d"), "-nldi-url", f.nldi.URL, "-nwps-url", f.nwps.URL}
	if code := run(args, &out, &errb); code != 0 {
		t.Errorf("exit %d: %s", code, errb.String())
	}
	if code := run([]string{"record"}, &out, &errb); code != 2 {
		t.Errorf("missing flags: exit %d", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("bad command: exit %d", code)
	}
}

func TestRecordSurfacesSeriesError(t *testing.T) {
	f := newFakeUpstreams(t)
	var out bytes.Buffer
	// 44444444's short_range series fails with NWPS's "Data unavailable".
	_ = record(ctx, f.config(t.TempDir(), "44444444"), &out)
	s := out.String()
	if !strings.Contains(s, "short_range") || !strings.Contains(s, "data unavailable") || strings.Contains(s, "no short-range forecast") {
		t.Errorf("want the series error in the output, got:\n%s", s)
	}
}

func TestRecordEmptySeriesMessage(t *testing.T) {
	nwps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"reach":{"reachId":"1001","name":"R"},"shortRange":null}`)
	}))
	defer nwps.Close()
	f := newFakeUpstreams(t)
	cfg := f.config(t.TempDir(), "11111111")
	cfg.NWPSURL = nwps.URL
	var out bytes.Buffer
	_ = record(ctx, cfg, &out)
	if !strings.Contains(out.String(), "no short-range forecast for comid 1001") {
		t.Errorf("output:\n%s", out.String())
	}
}
