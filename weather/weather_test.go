package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testUA = "(example.com, contact@example.com)"

var testNow = time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC)

// Test point; stations sit due north of it, 0.1 degrees of latitude (about
// 11.1 km) apart.
const (
	ptLat = 34.0
	ptLon = -84.0
)

type fakeStation struct {
	id   string
	lat  float64
	obs  []map[string]any // feature properties, any order
	elev any
}

// fakeNWS serves the endpoints Client uses.
type fakeNWS struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	stations []fakeStation
	hits     map[string]int
	agents   []string
	periods  []map[string]any
	hourly   []map[string]any
	pointsAt func(w http.ResponseWriter, r *http.Request) bool // optional override
}

func newFake(t *testing.T) *fakeNWS {
	f := &fakeNWS{t: t, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNWS) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, v := range f.hits {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

func (f *fakeNWS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.agents = append(f.agents, r.Header.Get("User-Agent"))
	f.hits[r.URL.Path]++
	f.mu.Unlock()
	if f.pointsAt != nil && f.pointsAt(w, r) {
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/points/"):
		writeJSON(w, map[string]any{"properties": map[string]any{
			"forecast":            f.srv.URL + "/gridpoints/TST/1,1/forecast",
			"forecastHourly":      f.srv.URL + "/gridpoints/TST/1,1/forecast/hourly",
			"observationStations": f.srv.URL + "/gridpoints/TST/1,1/stations",
			"timeZone":            "America/New_York",
		}})
	case p == "/gridpoints/TST/1,1/stations":
		var fs []any
		for _, s := range f.stations {
			fs = append(fs, map[string]any{
				"geometry": map[string]any{"coordinates": []float64{ptLon, s.lat}},
				"properties": map[string]any{"stationIdentifier": s.id, "name": "Station " + s.id,
					"elevation": s.elev},
			})
		}
		writeJSON(w, map[string]any{"features": fs})
	case strings.HasPrefix(p, "/stations/") && strings.HasSuffix(p, "/observations"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/stations/"), "/observations")
		for _, s := range f.stations {
			if s.id == id {
				var fs []any
				for _, o := range s.obs {
					fs = append(fs, map[string]any{"properties": o})
				}
				writeJSON(w, map[string]any{"features": fs})
				return
			}
		}
		http.NotFound(w, r)
	case p == "/gridpoints/TST/1,1/forecast":
		if r.URL.Query().Get("units") != "si" {
			f.t.Errorf("forecast without units=si: %s", r.URL)
		}
		writeJSON(w, map[string]any{"properties": map[string]any{
			"updateTime": "2026-11-04T18:26:49+00:00", "periods": f.periods}})
	case p == "/gridpoints/TST/1,1/forecast/hourly":
		writeJSON(w, map[string]any{"properties": map[string]any{
			"updateTime": "2026-11-04T18:26:49+00:00", "periods": f.hourly}})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/geo+json")
	b, _ := json.Marshal(v)
	w.Write(b)
}

func (f *fakeNWS) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(&Options{UserAgent: testUA, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	return c
}

func qty(v any, unit string) map[string]any {
	return map[string]any{"value": v, "unitCode": unit}
}

// obsAt is an observation `ago` before testNow with a temperature, which is
// all the staleness and ordering tests need.
func obsAt(ago time.Duration, extra map[string]any) map[string]any {
	o := map[string]any{
		"timestamp":       testNow.Add(-ago).Format(time.RFC3339),
		"textDescription": "Clear",
		"temperature":     qty(10.0, "wmoUnit:degC"),
	}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

// stationsAt makes fresh stations s0, s1, ... at the given offsets (in units
// of 0.1 degree of latitude north of the point).
func stationsAt(offsets ...int) []fakeStation {
	var out []fakeStation
	for i, o := range offsets {
		out = append(out, fakeStation{id: fmt.Sprintf("S%d", i), lat: ptLat + float64(o)/10,
			elev: qty(100.0, "wmoUnit:m"), obs: []map[string]any{obsAt(10*time.Minute, nil)}})
	}
	return out
}

func ids(obs []Observation) []string {
	var out []string
	for _, o := range obs {
		out = append(out, o.StationID)
	}
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestNewClientRequiresUserAgent(t *testing.T) {
	for _, opts := range []*Options{nil, {}, {UserAgent: "  \t"}} {
		if _, err := NewClient(opts); err == nil {
			t.Errorf("NewClient(%+v) succeeded, want error", opts)
		}
	}
	if _, err := NewClient(&Options{UserAgent: testUA}); err != nil {
		t.Errorf("NewClient with user agent: %v", err)
	}
}

func TestUserAgentSentOnEveryRequest(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1)
	f.periods = []map[string]any{}
	c := f.client(t)
	if _, err := c.Current(context.Background(), ptLat, ptLon); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Forecast(context.Background(), ptLat, ptLon); err != nil {
		t.Fatal(err)
	}
	if len(f.agents) < 5 {
		t.Fatalf("only %d requests seen", len(f.agents))
	}
	for _, a := range f.agents {
		if a != testUA {
			t.Errorf("User-Agent = %q, want %q", a, testUA)
		}
	}
}

func TestPointNotFoundIsNoCoverage(t *testing.T) {
	f := newFake(t)
	f.pointsAt = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/points/") {
			http.Error(w, `{"title":"Data Unavailable For Requested Point"}`, http.StatusNotFound)
			return true
		}
		return false
	}
	_, err := f.client(t).Current(context.Background(), 0, 0)
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 || e.Op != "points" {
		t.Fatalf("err = %v, want *Error 404 points", err)
	}
	if !errors.Is(err, ErrNoCoverage) {
		t.Errorf("err = %v, want ErrNoCoverage", err)
	}
	if _, err := f.client(t).Forecast(context.Background(), 0, 0); !errors.Is(err, ErrNoCoverage) {
		t.Errorf("Forecast err = %v, want ErrNoCoverage", err)
	}
}

func TestNullVersusZeroAndUnits(t *testing.T) {
	f := newFake(t)
	f.stations = []fakeStation{{id: "A", lat: ptLat + 0.1, elev: qty(305.1, "wmoUnit:m"), obs: []map[string]any{
		obsAt(5*time.Minute, map[string]any{
			"temperature":        qty(0, "wmoUnit:degC"),
			"dewpoint":           qty(nil, "wmoUnit:degC"),
			"relativeHumidity":   qty(93.5, "wmoUnit:percent"),
			"windSpeed":          qty(10.0, "wmoUnit:m_s-1"),
			"windGust":           qty(5.0, "wmoUnit:furlong_fortnight-1"),
			"windDirection":      qty(160, "wmoUnit:degree_(angle)"),
			"barometricPressure": qty(101286.88, "wmoUnit:Pa"),
			"timestamp":          testNow.Add(-5 * time.Minute).Format(time.RFC3339),
		}),
	}}}
	// Precipitation: 0 reported, newest report is routine (minute 55).
	f.stations[0].obs[0]["timestamp"] = time.Date(2026, 11, 4, 19, 55, 0, 0, time.UTC).Format(time.RFC3339)
	f.stations[0].obs[0]["precipitationLastHour"] = qty(0, "wmoUnit:mm")
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	o := got[0]
	if o.TemperatureC == nil || *o.TemperatureC != 0 {
		t.Errorf("TemperatureC = %v, want 0", o.TemperatureC)
	}
	if o.DewpointC != nil {
		t.Errorf("DewpointC = %v, want nil", *o.DewpointC)
	}
	if o.WindGustKmh != nil {
		t.Errorf("WindGustKmh = %v, want nil (unknown unit)", *o.WindGustKmh)
	}
	if o.WindSpeedKmh == nil || math.Abs(*o.WindSpeedKmh-36) > 1e-9 {
		t.Errorf("WindSpeedKmh = %v, want 36", o.WindSpeedKmh)
	}
	if o.PressureHPa == nil || math.Abs(*o.PressureHPa-1012.8688) > 1e-6 {
		t.Errorf("PressureHPa = %v, want 1012.8688", o.PressureHPa)
	}
	if o.HumidityPct == nil || *o.HumidityPct != 93.5 || o.WindDirectionDeg == nil || *o.WindDirectionDeg != 160 {
		t.Errorf("humidity/direction = %v / %v", o.HumidityPct, o.WindDirectionDeg)
	}
	if o.PrecipLast1hMm == nil || *o.PrecipLast1hMm != 0 {
		t.Errorf("PrecipLast1hMm = %v, want 0", o.PrecipLast1hMm)
	}
	if o.PrecipLast6hMm == nil || *o.PrecipLast6hMm != 0 || o.PrecipLast6hMissingHours != 5 ||
		o.PrecipLast24hMm == nil || *o.PrecipLast24hMm != 0 || o.PrecipLast24hMissingHours != 23 {
		t.Errorf("6h/24h = %v(+%d)/%v(+%d), want 0(+5)/0(+23) with one routine report",
			o.PrecipLast6hMm, o.PrecipLast6hMissingHours, o.PrecipLast24hMm, o.PrecipLast24hMissingHours)
	}
	if o.ElevationM == nil || *o.ElevationM != 305.1 || o.StationName != "Station A" {
		t.Errorf("elevation/name = %v / %q", o.ElevationM, o.StationName)
	}
	if o.DistanceKm < 11 || o.DistanceKm > 11.2 {
		t.Errorf("DistanceKm = %v, want about 11.1", o.DistanceKm)
	}
}

func TestConvert(t *testing.T) {
	v := func(x float64) *float64 { return &x }
	tests := []struct {
		name string
		m    *measure
		k    unitKind
		want *float64
	}{
		{"Pa", &measure{v(101325), "wmoUnit:Pa"}, unitPressure, v(1013.25)},
		{"m/s", &measure{v(2), "wmoUnit:m_s-1"}, unitSpeed, v(7.2)},
		{"km/h", &measure{v(7), "wmoUnit:km_h-1"}, unitSpeed, v(7)},
		{"m", &measure{v(0.0025), "wmoUnit:m"}, unitPrecip, v(2.5)},
		{"degF", &measure{v(212), "wmoUnit:degF"}, unitTemp, v(100)},
		{"unknown", &measure{v(1), "wmoUnit:kt"}, unitSpeed, nil},
		{"wrong kind", &measure{v(1), "wmoUnit:degC"}, unitPressure, nil},
		{"null", &measure{nil, "wmoUnit:degC"}, unitTemp, nil},
		{"absent", nil, unitTemp, nil},
	}
	for _, tt := range tests {
		got := tt.m.convert(tt.k)
		if (got == nil) != (tt.want == nil) || (got != nil && math.Abs(*got-*tt.want) > 1e-9) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStationsSortedAndFiltered(t *testing.T) {
	f := newFake(t)
	// Offsets of 3, 1, 6 (66 km, too far), 2 tenths of a degree; server order
	// is deliberately not by distance.
	f.stations = stationsAt(3, 1, 6, 2)
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"S1", "S3", "S0"}; !eq(ids(got), want) {
		t.Errorf("stations = %v, want %v", ids(got), want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].DistanceKm < got[i-1].DistanceKm {
			t.Errorf("distances not ascending: %v", got)
		}
	}
}

func TestStaleStationsSkippedAndCapped(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1, 2, 3, 4, 5, 1, 1, 1) // all within 50 km
	f.stations[0].obs = []map[string]any{obsAt(3*time.Hour, nil)}
	f.stations[1].obs = nil
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d stations %v, want 3", len(got), ids(got))
	}
	for _, o := range got {
		if o.StationID == "S0" || o.StationID == "S1" {
			t.Errorf("stale station %s returned", o.StationID)
		}
	}
}

func TestNextBatchWhenFirstIsStale(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1, 1, 1, 1, 1, 2, 2) // 7 stations
	for i := range 5 {
		f.stations[i].obs = []map[string]any{obsAt(5*time.Hour, nil)}
	}
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"S5", "S6"}; !eq(ids(got), want) {
		t.Errorf("stations = %v, want %v", ids(got), want)
	}
	// With enough fresh stations in the first batch, the second isn't queried.
	f2 := newFake(t)
	f2.stations = stationsAt(1, 1, 1, 1, 1, 2, 2)
	if _, err := f2.client(t).Current(context.Background(), ptLat, ptLon); err != nil {
		t.Fatal(err)
	}
	if n := f2.count("/stations/S"); n != 5 {
		t.Errorf("observation requests = %d, want 5", n)
	}
}

func TestNoStationsInRangeIsNotAnError(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(9)
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want empty, nil", got, err)
	}
}

func TestStationFailureSkippedUnlessAllFail(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1, 2)
	f.pointsAt = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/stations/S0/observations" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil || !eq(ids(got), []string{"S1"}) {
		t.Errorf("got %v, %v; want S1", ids(got), err)
	}
	f.stations = stationsAt(1)
	_, err = f.client(t).Current(context.Background(), ptLat, ptLon)
	var e *Error
	if !errors.As(err, &e) || e.Op != "observations" || e.Status != 500 {
		t.Errorf("err = %v, want observations 500", err)
	}
}

// hourlyPrecip builds routine reports at :53 for each of the last n hours
// ending at 19:53, each reporting mm, plus a special and a 5-minute report in
// every hour that would corrupt a sum that included them.
func hourlyPrecip(n int, mm float64, skipHour int) []map[string]any {
	var obs []map[string]any
	for h := range n {
		base := time.Date(2026, 11, 4, 19, 0, 0, 0, time.UTC).Add(-time.Duration(h) * time.Hour)
		at := func(min int, extra map[string]any) map[string]any {
			o := map[string]any{"timestamp": base.Add(time.Duration(min) * time.Minute).Format(time.RFC3339)}
			for k, v := range extra {
				o[k] = v
			}
			return o
		}
		obs = append(obs, at(55, nil)) // 5-minute report: no precip key
		if h != skipHour {
			obs = append(obs, at(53, map[string]any{"precipitationLastHour": qty(mm, "wmoUnit:mm")}))
		}
		obs = append(obs, at(31, map[string]any{"precipitationLastHour": qty(99.0, "wmoUnit:mm")})) // special
	}
	return obs
}

func TestPrecipTotals(t *testing.T) {
	f := newFake(t)
	f.stations = []fakeStation{
		{id: "FULL", lat: ptLat + 0.1, obs: hourlyPrecip(26, 0.5, -1)},
		{id: "GAP", lat: ptLat + 0.2, obs: hourlyPrecip(26, 0.5, 10)}, // hour 10 missing
		{id: "GAP3", lat: ptLat + 0.3, obs: hourlyPrecip(26, 0.5, 3)}, // hour 3 missing
	}
	got, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
	val := func(p *float64) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p)
	}
	check := func(o Observation, h1, h6, h24 string, m6, m24 int) {
		t.Helper()
		if a, b, c := val(o.PrecipLast1hMm), val(o.PrecipLast6hMm), val(o.PrecipLast24hMm); a != h1 || b != h6 || c != h24 ||
			o.PrecipLast6hMissingHours != m6 || o.PrecipLast24hMissingHours != m24 {
			t.Errorf("%s: 1h/6h/24h = %s/%s(+%d)/%s(+%d), want %s/%s(+%d)/%s(+%d)", o.StationID, a, b,
				o.PrecipLast6hMissingHours, c, o.PrecipLast24hMissingHours, h1, h6, m6, h24, m24)
		}
	}
	check(got[0], "0.5", "3", "12", 0, 0)
	check(got[1], "0.5", "3", "11.5", 0, 1)
	check(got[2], "0.5", "2.5", "11.5", 1, 1)
}

func TestPrecipNullHours(t *testing.T) {
	routine := func(raw string, v *float64) []report {
		p := obsProps{RawMessage: raw, Precip1h: &measure{v, "wmoUnit:mm"}}
		return []report{{time.Date(2026, 11, 4, 19, 53, 0, 0, time.UTC), p}}
	}
	const ao2 = "KXXX 041953Z AUTO 21009KT 10SM CLR 22/13 A3007 RMK AO2 SLP18"
	tests := []struct {
		name string
		raw  string
		want bool // 1h known as 0
	}{
		{"AO2", ao2, true},
		{"AO2A", "KXXX 041953Z 21009KT 10SM CLR 22/13 A3007 RMK AO2A SLP18", true},
		{"AO2 with PNO", ao2 + " PNO", false},
		{"AO1", "KXXX 041953Z 21009KT 10SM CLR 22/13 A3007 RMK AO1", false},
		{"no AO marker", "KXXX 041953Z 21009KT 10SM CLR 22/13 A3007", false},
		{"empty raw", "", false},
	}
	for _, tt := range tests {
		pr := precipTotals(routine(tt.raw, nil), testNow)
		if got := pr.h1 != nil && *pr.h1 == 0; got != tt.want {
			t.Errorf("%s: 1h = %v, want known zero %v", tt.name, pr.h1, tt.want)
		}
		if tt.want && (pr.h6 == nil || pr.miss6 != 5 || pr.miss24 != 23) {
			t.Errorf("%s: 6h = %v miss %d/%d", tt.name, pr.h6, pr.miss6, pr.miss24)
		}
		if !tt.want && (pr.h6 != nil || pr.h24 != nil || pr.miss6 != 6 || pr.miss24 != 24) {
			t.Errorf("%s: all unknown gave 6h %v 24h %v miss %d/%d", tt.name, pr.h6, pr.h24, pr.miss6, pr.miss24)
		}
	}

	// A mixed window: rain in one hour, dry (AO2 null) in another, unknown
	// (empty raw) in a third, nothing for the rest.
	hr := func(h int, raw string, v *float64) report {
		r := routine(raw, v)[0]
		r.t = r.t.Add(-time.Duration(h) * time.Hour)
		return r
	}
	rs := []report{hr(0, ao2, ptr(1.5)), hr(1, ao2, nil), hr(2, "", nil)}
	pr := precipTotals(rs, testNow)
	if pr.h6 == nil || *pr.h6 != 1.5 || pr.miss6 != 4 || pr.h24 == nil || *pr.h24 != 1.5 || pr.miss24 != 22 {
		t.Errorf("mixed: 6h %v(+%d) 24h %v(+%d), want 1.5(+4) / 1.5(+22)", pr.h6, pr.miss6, pr.h24, pr.miss24)
	}
}

func TestPrecipAnchorAge(t *testing.T) {
	// A station whose last routine report is 20 hours old knows nothing now.
	p := obsProps{RawMessage: "KXXX RMK AO2", Precip1h: &measure{ptr(1.0), "wmoUnit:mm"}}
	rs := []report{{time.Date(2026, 11, 3, 23, 53, 0, 0, time.UTC), p}}
	pr := precipTotals(rs, testNow)
	if pr.h1 != nil || pr.h6 != nil || pr.h24 != nil || pr.miss6 != 6 || pr.miss24 != 24 {
		t.Errorf("stale anchor: %+v, want all unknown", pr)
	}
}

func TestPrecipPrefersReportWithRawMessage(t *testing.T) {
	// A later 5-minute report carries the key as a null with no raw message;
	// the :53 METAR in the same hour must win.
	metar := report{time.Date(2026, 11, 4, 19, 53, 0, 0, time.UTC),
		obsProps{RawMessage: "KXXX RMK AO2 P0012", Precip1h: &measure{ptr(3.0), "wmoUnit:mm"}}}
	short := report{time.Date(2026, 11, 4, 19, 55, 0, 0, time.UTC),
		obsProps{Precip1h: &measure{nil, "wmoUnit:mm"}}}
	pr := precipTotals([]report{short, metar}, testNow)
	if pr.h1 == nil || *pr.h1 != 3 {
		t.Errorf("1h = %v, want 3", pr.h1)
	}
	// Among equals the latest wins.
	later := metar
	later.t = later.t.Add(2 * time.Minute)
	later.p.Precip1h = &measure{ptr(4.0), "wmoUnit:mm"}
	if pr := precipTotals([]report{later, metar}, testNow); pr.h1 == nil || *pr.h1 != 4 {
		t.Errorf("1h = %v, want 4", pr.h1)
	}
}

func TestPointCacheEvictsExpired(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1)
	c := f.client(t)
	now := testNow
	c.now = func() time.Time { return now }
	if _, err := c.Current(context.Background(), ptLat, ptLon); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Hour)
	if _, err := c.Current(context.Background(), ptLat+0.5, ptLon); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cache[pointKey(ptLat, ptLon)]; ok || len(c.cache) != 1 {
		t.Errorf("cache has %d entries, want only the new one", len(c.cache))
	}
}

func ptr(v float64) *float64 { return &v }

func TestPressureTrend(t *testing.T) {
	mk := func(newest float64, older map[time.Duration]float64) []report {
		rs := []report{{testNow, obsProps{Barometric: &measure{ptr(newest * 100), "wmoUnit:Pa"}}}}
		for ago, v := range older {
			rs = append(rs, report{testNow.Add(-ago), obsProps{Barometric: &measure{ptr(v * 100), "wmoUnit:Pa"}}})
		}
		return rs
	}
	pick := func(p obsProps) *float64 { return p.Barometric.convert(unitPressure) }
	tests := []struct {
		name   string
		rs     []report
		trend  string
		change float64
	}{
		{"rising", mk(1015, map[time.Duration]float64{3 * time.Hour: 1012}), "rising", 3},
		{"falling", mk(1008, map[time.Duration]float64{3*time.Hour + 20*time.Minute: 1011.5}), "falling", -3.5},
		{"steady", mk(1010.5, map[time.Duration]float64{3 * time.Hour: 1010}), "steady", 0.5},
		{"picks closest", mk(1010, map[time.Duration]float64{2*time.Hour + 45*time.Minute: 1000, 3*time.Hour + 5*time.Minute: 1011}), "falling", -1},
		{"too far", mk(1010, map[time.Duration]float64{2 * time.Hour: 1000, 4 * time.Hour: 1000}), "", 0},
		{"no history", mk(1010, nil), "", 0},
	}
	for _, tt := range tests {
		trend, change := pressureTrend(tt.rs, pick)
		if trend != tt.trend {
			t.Errorf("%s: trend = %q, want %q", tt.name, trend, tt.trend)
		}
		if tt.trend == "" {
			if change != nil {
				t.Errorf("%s: change = %v, want nil", tt.name, *change)
			}
		} else if change == nil || math.Abs(*change-tt.change) > 1e-9 {
			t.Errorf("%s: change = %v, want %v", tt.name, change, tt.change)
		}
	}
}

func TestPressureFallsBackToSeaLevel(t *testing.T) {
	st := station{id: "X"}
	rs := []report{
		{testNow.Add(-5 * time.Minute), obsProps{SeaLevel: &measure{ptr(101500), "wmoUnit:Pa"}}},
		{testNow.Add(-3*time.Hour - 5*time.Minute), obsProps{SeaLevel: &measure{ptr(101200), "wmoUnit:Pa"}}},
	}
	o := summarize(st, 1, rs, testNow)
	if o == nil || o.PressureHPa == nil || *o.PressureHPa != 1015 || o.PressureTrend != "rising" {
		t.Errorf("got %+v", o)
	}
}

func TestPointCache(t *testing.T) {
	f := newFake(t)
	f.stations = stationsAt(1)
	c := f.client(t)
	now := testNow
	c.now = func() time.Time { return now }
	ctx := context.Background()
	for range 2 {
		if _, err := c.Current(ctx, ptLat, ptLon); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Forecast(ctx, ptLat, ptLon); err != nil {
		t.Fatal(err)
	}
	if n, s := f.count("/points/"), f.count("/gridpoints/TST/1,1/stations"); n != 1 || s != 1 {
		t.Errorf("points/stations requests = %d/%d, want 1/1", n, s)
	}
	// The same point to 4 decimals shares an entry.
	if _, err := c.Current(ctx, ptLat+0.00001, ptLon); err != nil {
		t.Fatal(err)
	}
	if n := f.count("/points/"); n != 1 {
		t.Errorf("points requests = %d, want 1", n)
	}
	now = now.Add(23 * time.Hour)
	c.Current(ctx, ptLat, ptLon)
	if n := f.count("/points/"); n != 1 {
		t.Errorf("points requests after 23h = %d, want 1", n)
	}
	now = now.Add(2 * time.Hour)
	c.Current(ctx, ptLat, ptLon)
	if n, s := f.count("/points/"), f.count("/gridpoints/TST/1,1/stations"); n != 2 || s != 2 {
		t.Errorf("points/stations requests after 25h = %d/%d, want 2/2", n, s)
	}
}

func TestOffHostLinkRejected(t *testing.T) {
	f := newFake(t)
	f.pointsAt = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/points/") {
			writeJSON(w, map[string]any{"properties": map[string]any{
				"forecast":            "https://evil.example.com/forecast",
				"forecastHourly":      f.srv.URL + "/h",
				"observationStations": f.srv.URL + "/s",
			}})
			return true
		}
		return false
	}
	_, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Error(), "not on") {
		t.Errorf("err = %v, want off-host *Error", err)
	}
	if f.count("/s") > 0 || f.count("/h") > 0 {
		t.Error("followed links after rejecting the response")
	}
}

func TestNon200(t *testing.T) {
	f := newFake(t)
	f.pointsAt = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return true
	}
	_, err := f.client(t).Forecast(context.Background(), ptLat, ptLon)
	var e *Error
	if !errors.As(err, &e) || e.Status != 503 || e.Timeout() {
		t.Fatalf("err = %v, want *Error 503", err)
	}
	if !strings.HasPrefix(e.Error(), "weather: points: ") {
		t.Errorf("message = %q", e.Error())
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t)
	f.pointsAt = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "slow", http.StatusGatewayTimeout)
		return true
	}
	_, err := f.client(t).Current(context.Background(), ptLat, ptLon)
	var e *Error
	if !errors.As(err, &e) || !e.Timeout() {
		t.Errorf("504: err = %v, want Timeout", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	c, _ := NewClient(&Options{UserAgent: testUA, BaseURL: slow.URL,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}})
	_, err = c.Current(context.Background(), ptLat, ptLon)
	if !errors.As(err, &e) || !e.Timeout() || e.Status != 0 {
		t.Errorf("client timeout: err = %v, want Timeout", err)
	}
}

func TestForecast(t *testing.T) {
	f := newFake(t)
	period := func(name string, start time.Time, hours int, temp any, unit string, pop any) map[string]any {
		return map[string]any{"name": name, "startTime": start.Format(time.RFC3339),
			"endTime": start.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339), "isDaytime": true,
			"temperature": temp, "temperatureUnit": unit,
			"probabilityOfPrecipitation": qty(pop, "wmoUnit:percent"),
			"windSpeed":                  "5 to 10 km/h", "windDirection": "NW",
			"shortForecast": "Sunny", "detailedForecast": "Sunny, with a high near 20."}
	}
	f.periods = []map[string]any{
		period("Today", testNow, 12, 20, "C", 70),
		period("Tonight", testNow.Add(12*time.Hour), 12, 50, "F", nil),
	}
	// Hourly: one already over, one in progress, then 30 upcoming.
	f.hourly = append(f.hourly, period("", testNow.Add(-2*time.Hour), 1, 9, "C", 0))
	f.hourly = append(f.hourly, period("", testNow.Add(-30*time.Minute), 1, 10, "C", 0))
	for h := 1; h <= 30; h++ {
		f.hourly = append(f.hourly, period("", testNow.Add(time.Duration(h)*time.Hour), 1, 10+h, "C", 0))
	}
	fc, err := f.client(t).Forecast(context.Background(), ptLat, ptLon)
	if err != nil {
		t.Fatal(err)
	}
	if fc.TimeZone != "America/New_York" || !fc.Updated.Equal(time.Date(2026, 11, 4, 18, 26, 49, 0, time.UTC)) {
		t.Errorf("tz/updated = %q / %v", fc.TimeZone, fc.Updated)
	}
	if len(fc.Periods) != 2 {
		t.Fatalf("periods = %d", len(fc.Periods))
	}
	p := fc.Periods[0]
	if p.Name != "Today" || p.WindSpeed != "5 to 10 km/h" || p.WindDirection != "NW" || p.Summary != "Sunny" ||
		p.Detail == "" || !p.IsDaytime || *p.TemperatureC != 20 || *p.PrecipChancePct != 70 {
		t.Errorf("period = %+v", p)
	}
	if q := fc.Periods[1]; math.Abs(*q.TemperatureC-10) > 1e-9 || q.PrecipChancePct != nil {
		t.Errorf("F period: temp %v pop %v, want 10 / nil", *q.TemperatureC, q.PrecipChancePct)
	}
	if len(fc.Hourly) != 24 {
		t.Fatalf("hourly = %d, want 24", len(fc.Hourly))
	}
	if fc.Hourly[0].Start != testNow.Add(-30*time.Minute) || *fc.Hourly[23].TemperatureC != 33 {
		t.Errorf("hourly window: first %v, last temp %v", fc.Hourly[0].Start, *fc.Hourly[23].TemperatureC)
	}
}

func TestInvalidCoordinates(t *testing.T) {
	f := newFake(t)
	for _, ll := range [][2]float64{{91, 0}, {0, 181}, {math.NaN(), 0}} {
		if _, err := f.client(t).Current(context.Background(), ll[0], ll[1]); err == nil {
			t.Errorf("Current(%v) succeeded", ll)
		}
	}
	if f.count("/") != 0 {
		t.Error("made requests for invalid coordinates")
	}
}

func TestHaversine(t *testing.T) {
	if d := haversineKm(34, -84, 35, -84); math.Abs(d-111.19) > 0.1 {
		t.Errorf("1 degree of latitude = %v km, want about 111.2", d)
	}
	if d := haversineKm(34, -84, 34, -84); d != 0 {
		t.Errorf("same point = %v", d)
	}
}

// TestLive hits the real NWS API. It needs a User-Agent in NWS_USER_AGENT,
// e.g. "(example.com, contact@example.com)" with real contact details.
func TestLive(t *testing.T) {
	if os.Getenv("TRIBUTARY_LIVE") == "" {
		t.Skip("set TRIBUTARY_LIVE=1 to hit the real NWS API")
	}
	ua := os.Getenv("NWS_USER_AGENT")
	if ua == "" {
		t.Skip("set NWS_USER_AGENT to a User-Agent such as \"(app, contact)\"")
	}
	c, err := NewClient(&Options{UserAgent: ua})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	obs, err := c.Current(ctx, 34.42, -84.22)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) == 0 {
		t.Fatal("no stations")
	}
	for _, o := range obs {
		t.Logf("%s %q %.1f km temp=%v hum=%v p=%v %s 1h=%v 6h=%v 24h=%v", o.StationID, o.StationName,
			o.DistanceKm, o.TemperatureC != nil, o.HumidityPct != nil, o.PressureHPa != nil,
			o.PressureTrend, o.PrecipLast1hMm != nil, o.PrecipLast6hMm != nil, o.PrecipLast24hMm != nil)
	}
	fc, err := c.Forecast(ctx, 34.42, -84.22)
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Periods) < 10 || len(fc.Hourly) < 20 || fc.TimeZone == "" {
		t.Errorf("forecast: %d periods, %d hourly, tz %q", len(fc.Periods), len(fc.Hourly), fc.TimeZone)
	}
}
