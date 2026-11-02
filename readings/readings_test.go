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

func meta(series, name, subloc, desc string) string {
	null := func(s string) string {
		if s == "" {
			return "null"
		}
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"id":%q,"parameter_name":%q,"sublocation_identifier":%s,"web_description":%s}`,
		series, name, null(subloc), null(desc))
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
		))
	})
	mux.HandleFunc("/collections/time-series-metadata/items", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, fc("",
			meta("a1", "Discharge", "", ""),
			meta("a2", "Temperature, water", "From multiparameter sonde", ""),
			meta("a3", "Temperature, water", "1.0 ft from riverbed (bottom)", "Discontinued"),
			meta("b1", "Gage height", "", ""),
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
