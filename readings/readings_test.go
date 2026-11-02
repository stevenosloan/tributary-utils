package readings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func fc(next string, props ...string) string {
	feats := make([]string, len(props))
	for i, p := range props {
		feats[i] = `{"type":"Feature","properties":` + p + `}`
	}
	links := `[]`
	if next != "" {
		links = `[{"rel":"next","href":"` + next + `"}]`
	}
	return `{"type":"FeatureCollection","features":[` + strings.Join(feats, ",") + `],"links":` + links + `}`
}

func value(series, site, code, val string) string {
	return fmt.Sprintf(`{"time_series_id":%q,"monitoring_location_id":"USGS-%s","parameter_code":%q,"time":"2026-11-02T21:45:00+00:00","value":%s,"unit_of_measure":"u","approval_status":"Provisional","qualifier":null}`,
		series, site, code, val)
}

func meta(series, site, name, subloc, desc string) string {
	null := func(s string) string {
		if s == "" {
			return "null"
		}
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"id":%q,"monitoring_location_id":"USGS-%s","parameter_name":%q,"sublocation_identifier":%s,"web_description":%s}`,
		series, site, name, null(subloc), null(desc))
}

// fakeAPI serves two gauges: "a" with discharge, two water temperature
// sensors (one discontinued) and a sentinel value; "b" with gage height,
// split over two pages. "empty" has no series.
func fakeAPI(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []*http.Request
	)
	record := func(r *http.Request) { mu.Lock(); reqs = append(reqs, r); mu.Unlock() }
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/collections/latest-continuous/items", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.URL.Query().Get("cursor") == "2" {
			fmt.Fprint(w, fc("", value("b1", "b", "00065", `"3.1"`)))
			return
		}
		fmt.Fprint(w, fc(srv.URL+"/collections/latest-continuous/items?cursor=2",
			value("a2", "a", "00010", `"12.5"`),
			value("a1", "a", "00060", `"240"`),
			value("a3", "a", "00010", `"24.3"`),
			value("a4", "a", "00095", `"-999999"`),
			value("a5", "a", "00300", `null`),
			value("a6", "a", "00400", `"NaN"`),
		))
	})
	mux.HandleFunc("/collections/time-series-metadata/items", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, fc("",
			meta("a1", "a", "Discharge", "", ""),
			meta("a2", "a", "Temperature, water", "From multiparameter sonde", ""),
			meta("a3", "a", "Temperature, water", "1.0 ft from riverbed (bottom)", "Discontinued 2017"),
			meta("b1", "b", "Gage height", "", ""),
		))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestLatest(t *testing.T) {
	srv, reqs := fakeAPI(t)
	c := NewClient(&Options{BaseURL: srv.URL, APIKey: "k"})
	got, err := c.Latest(context.Background(), []string{"a", "USGS-b", "empty"})
	if err != nil {
		t.Fatal(err)
	}

	type row struct {
		code, name, subloc string
		value              float64
		discontinued       bool
	}
	want := map[string][]row{
		"a": {
			{"00010", "Temperature, water", "From multiparameter sonde", 12.5, false},
			{"00010", "Temperature, water", "1.0 ft from riverbed (bottom)", 24.3, true},
			{"00060", "Discharge", "", 240, false},
		},
		"b": {{"00065", "Gage height", "", 3.1, false}},
	}
	if len(got) != len(want) {
		t.Fatalf("got sites %v, want a and b", got)
	}
	for site, rows := range want {
		rs := got[site]
		if len(rs) != len(rows) {
			t.Fatalf("%s: got %d readings, want %d: %+v", site, len(rs), len(rows), rs)
		}
		for i, w := range rows {
			r := rs[i]
			g := row{r.ParameterCode, r.Parameter, r.Sublocation, r.Value, r.Discontinued}
			if g != w {
				t.Errorf("%s[%d] = %+v, want %+v", site, i, g, w)
			}
		}
	}
	if r := got["a"][0]; r.Unit != "u" || r.Approval != "Provisional" ||
		!r.Time.Equal(time.Date(2026, 11, 2, 21, 45, 0, 0, time.UTC)) {
		t.Errorf("fields not carried over: %+v", r)
	}

	for _, r := range *reqs {
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("%s: missing API key header", r.URL.Path)
		}
		if r.URL.Query().Get("cursor") == "" &&
			r.URL.Query().Get("monitoring_location_id") != "USGS-a,USGS-b,USGS-empty" {
			t.Errorf("%s: sites = %q", r.URL.Path, r.URL.Query().Get("monitoring_location_id"))
		}
	}
}

func TestLatestNoSites(t *testing.T) {
	srv, reqs := fakeAPI(t)
	got, err := NewClient(&Options{BaseURL: srv.URL}).Latest(context.Background(), nil)
	if err != nil || len(got) != 0 || len(*reqs) != 0 {
		t.Fatalf("got %v, %v after %d requests; want empty, no requests", got, err, len(*reqs))
	}
}

func TestLatestErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		client  *http.Client
		status  int
		timeout bool
	}{
		{name: "bad status", status: http.StatusTooManyRequests,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "slow down", http.StatusTooManyRequests)
			}},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, timeout: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "timeout", http.StatusGatewayTimeout)
			}},
		{name: "client timeout", timeout: true, client: &http.Client{Timeout: 20 * time.Millisecond},
			handler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			}},
		{name: "bad json", status: http.StatusOK,
			handler: func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			c := NewClient(&Options{BaseURL: srv.URL, HTTPClient: tt.client})
			_, err := c.Latest(context.Background(), []string{"a"})
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("got %v, want *Error", err)
			}
			if re.Status != tt.status || re.Timeout() != tt.timeout {
				t.Errorf("got status %d timeout %v, want %d %v", re.Status, re.Timeout(), tt.status, tt.timeout)
			}
		})
	}
}

func TestLatestBatchesSites(t *testing.T) {
	var (
		mu    sync.Mutex
		asked = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("monitoring_location_id"), ",")
		if len(ids) > sitesPerRequest {
			t.Errorf("%s: %d sites in one request", r.URL.Path, len(ids))
		}
		mu.Lock()
		for _, id := range ids {
			asked[r.URL.Path+" "+id]++
		}
		mu.Unlock()
		fmt.Fprint(w, fc(""))
	}))
	defer srv.Close()

	sites := make([]string, 2*sitesPerRequest+50)
	for i := range sites {
		sites[i] = fmt.Sprintf("%08d", i)
	}
	if _, err := NewClient(&Options{BaseURL: srv.URL}).Latest(context.Background(), sites); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/collections/latest-continuous/items", "/collections/time-series-metadata/items"} {
		for _, s := range sites {
			if n := asked[path+" USGS-"+s]; n != 1 {
				t.Fatalf("%s: site %s asked %d times, want 1", path, s, n)
			}
		}
	}
}

func TestLatestNextLinks(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		fmt.Fprint(w, fc(""))
	}))
	defer other.Close()

	tests := []struct {
		name    string
		next    func(self string) string
		wantErr bool
	}{
		// Some servers repeat the current page as next on the last page.
		{name: "repeats", next: func(self string) string { return self }},
		{name: "other host", wantErr: true, next: func(string) string { return other.URL + "/items" }},
		{name: "other scheme", wantErr: true, next: func(self string) string { return strings.Replace(self, "http:", "https:", 1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var srv *httptest.Server
			pages := 0
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/collections/time-series-metadata/items" {
					fmt.Fprint(w, fc(""))
					return
				}
				pages++
				fmt.Fprint(w, fc(tt.next(srv.URL+r.URL.RequestURI()), value("a1", "a", "00060", `"240"`)))
			}))
			defer srv.Close()
			got, err := NewClient(&Options{BaseURL: srv.URL, APIKey: "k"}).Latest(context.Background(), []string{"a"})
			if tt.wantErr {
				var re *Error
				if !errors.As(err, &re) || re.Op != "latest-continuous" {
					t.Fatalf("got %v, want *Error for latest-continuous", err)
				}
			} else if err != nil || len(got["a"]) != 1 || pages != 1 {
				t.Fatalf("got %v, %v after %d pages; want one reading from one page", got, err, pages)
			}
			if elsewhere != 0 {
				t.Errorf("followed a next link to another host")
			}
		})
	}
}

func TestLatestCachesMetadata(t *testing.T) {
	var (
		mu       sync.Mutex
		metaReqs []string
		series   = []string{value("a1", "a", "00060", `"240"`)}
		metas    = []string{meta("a1", "a", "Discharge", "", "")}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/collections/time-series-metadata/items" {
			metaReqs = append(metaReqs, r.URL.Query().Get("monitoring_location_id"))
			fmt.Fprint(w, fc("", metas...))
			return
		}
		fmt.Fprint(w, fc("", series...))
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	c := NewClient(&Options{BaseURL: srv.URL})
	c.now = func() time.Time { return now }
	latest := func(sites ...string) map[string][]Reading {
		t.Helper()
		got, err := c.Latest(context.Background(), sites)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	wantMeta := func(want ...string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if fmt.Sprint(metaReqs) != fmt.Sprint(want) {
			t.Errorf("metadata requests = %q, want %q", metaReqs, want)
		}
		metaReqs = nil
	}

	latest("a", "b")
	wantMeta("USGS-a,USGS-b")

	// Cached, including b, which has no series.
	if got := latest("a", "b"); got["a"][0].Parameter != "Discharge" {
		t.Errorf("cached reading = %+v", got["a"][0])
	}
	wantMeta()

	// A new series at a refetches only that gauge.
	mu.Lock()
	series = append(series, value("a2", "a", "00065", `"3.1"`))
	metas = append(metas, meta("a2", "a", "Gage height", "", ""))
	mu.Unlock()
	if got := latest("a", "b"); len(got["a"]) != 2 || got["a"][1].Parameter != "Gage height" {
		t.Errorf("after new series = %+v", got["a"])
	}
	wantMeta("USGS-a")

	now = now.Add(metaTTL)
	latest("a", "b")
	wantMeta("USGS-a,USGS-b")
}

// Live check against the real Water Data API; run with TRIBUTARY_LIVE=1.
func TestLatestLive(t *testing.T) {
	if os.Getenv("TRIBUTARY_LIVE") == "" {
		t.Skip("set TRIBUTARY_LIVE=1 to hit the real Water Data API")
	}
	// Potomac River near Washington DC: many parameters, some discontinued.
	got, err := NewClient(nil).Latest(context.Background(), []string{"01646500"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got["01646500"] {
		t.Logf("%s %-28s %-32q %10v %-8s %s discontinued=%v", r.ParameterCode, r.Parameter, r.Sublocation,
			r.Value, r.Unit, r.Time.Format(time.RFC3339), r.Discontinued)
	}
	if len(got["01646500"]) == 0 {
		t.Fatal("no readings")
	}
}
