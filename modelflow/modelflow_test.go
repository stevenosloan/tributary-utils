package modelflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenosloan/tributary-utils/cache"
)

func init() { retryPause = time.Millisecond }

var ctx = context.Background()

const reachJSON = `"reach":{"reachId":"6495864","name":"Amicalola Creek","latitude":34.4119,"longitude":-84.213,"streamflow":["analysis_assimilation"]}`

func member(ref, units string, pts ...string) string {
	return fmt.Sprintf(`{"referenceTime":%q,"units":%q,"data":[%s]}`, ref, units, strings.Join(pts, ","))
}

func pt(t string, flow string) string {
	return fmt.Sprintf(`{"validTime":%q,"flow":%s}`, t, flow)
}

// body wraps one series object (field is e.g. "shortRange") in a response.
func body(field, obj string) string {
	return fmt.Sprintf(`{%s,%q:%s}`, reachJSON, field, obj)
}

var fields = map[string]string{
	"analysis_assimilation": "analysisAssimilation", "short_range": "shortRange",
	"medium_range": "mediumRange", "long_range": "longRange", "medium_range_blend": "mediumRangeBlend",
}

// fake serves per-series handlers and counts requests per series.
type fake struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   map[string]int
}

// newFake serves handler for each request; handler gets the series and the
// 1-based count of requests for it.
func newFake(t *testing.T, handler func(w http.ResponseWriter, series string, n int)) *fake {
	t.Helper()
	f := &fake{n: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/reaches/6495864/streamflow") {
			http.NotFound(w, r)
			return
		}
		s := r.URL.Query().Get("series")
		f.mu.Lock()
		f.n[s]++
		n := f.n[s]
		f.mu.Unlock()
		handler(w, s, n)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) count(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[s]
}

func (f *fake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := 0
	for _, n := range f.n {
		t += n
	}
	return t
}

// standard serves analysis, short range and empty medium range.
func standard(w http.ResponseWriter, s string, _ int) {
	switch s {
	case "analysis_assimilation":
		fmt.Fprint(w, body("analysisAssimilation", `{"series":`+member("2026-11-04T16:00:00Z", "ft³/s",
			pt("2026-11-02T22:00:00Z", "44.5"), pt("2026-11-02T20:00:00Z", "40"), pt("2026-11-02T21:00:00Z", "null"), pt("bad", "1"))+`}`))
	case "short_range":
		fmt.Fprint(w, body("shortRange", `{"series":`+member("2026-11-04T14:00:00Z", "ft³/s",
			pt("2026-11-04T15:00:00Z", "50"), pt("2026-11-04T16:00:00Z", "51.5"))+`}`))
	default:
		fmt.Fprint(w, body(fields[s], `{}`))
	}
}

func unavailableBody(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"code": 14, "message": "Data unavailable", "details": []}`)
}

// seriesErr returns the error for series s of a Flow call that must have
// succeeded as a call, failing the test otherwise.
func seriesErr(t *testing.T, res *Result, err error, s Series) error {
	t.Helper()
	if err != nil || res == nil {
		t.Fatalf("Flow error = %v, res = %v; want series errors only", err, res)
	}
	if res.Errors[s] == nil {
		t.Fatalf("no error for %s; Errors = %v", s, res.Errors)
	}
	return res.Errors[s]
}

func client(f *fake, o Options) *Client {
	o.BaseURL = f.srv.URL
	return NewClient(&o)
}

func TestFlow(t *testing.T) {
	f := newFake(t, standard)
	res, err := client(f, Options{}).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != nil {
		t.Errorf("Errors = %v", res.Errors)
	}
	if r := res.Reach; r.ID != 6495864 || r.Name != "Amicalola Creek" || r.Lat == nil || r.Lon == nil || *r.Lat != 34.4119 || *r.Lon != -84.213 {
		t.Errorf("Reach = %+v", r)
	}
	if res.AllFailed() {
		t.Error("AllFailed on success")
	}
	a := res.Run(Analysis)
	if a == nil || a.Member != "series" || a.Unit != "ft^3/s" || a.Series != Analysis {
		t.Fatalf("analysis = %+v", a)
	}
	if want := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC); !a.ReferenceTime.Equal(want) {
		t.Errorf("ReferenceTime = %v", a.ReferenceTime)
	}
	// Null flow and bad time dropped; the rest sorted.
	if len(a.Points) != 2 || a.Points[0].Flow != 40 || a.Points[1].Flow != 44.5 || !a.Points[0].Time.Before(a.Points[1].Time) {
		t.Errorf("points = %+v", a.Points)
	}
	s := res.Run(ShortRange)
	if s == nil || len(s.Points) != 2 || s.Points[1].Flow != 51.5 {
		t.Errorf("short range = %+v", s)
	}
	if _, ok := res.Runs[MediumRange]; ok {
		t.Error("unrequested series present")
	}
	if f.count("medium_range") != 0 {
		t.Error("unrequested series fetched")
	}
}

func TestMembersSorted(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, _ int) {
		fmt.Fprint(w, body("mediumRange", `{"member2":`+member("2026-11-04T12:00:00Z", "cfs", pt("2026-11-05T00:00:00Z", "2"))+
			`,"mean":`+member("2026-11-04T12:00:00Z", "cfs", pt("2026-11-05T00:00:00Z", "1"))+
			`,"member1":`+member("2026-11-04T12:00:00Z", "cfs", pt("2026-11-05T00:00:00Z", "3"))+
			`,"nodata":`+member("2026-11-04T12:00:00Z", "cfs")+`}`))
	})
	res, err := client(f, Options{Series: []Series{MediumRange}}).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	runs := res.Runs[MediumRange]
	var got []string
	for _, r := range runs {
		got = append(got, r.Member)
		if r.Unit != "ft^3/s" || r.Series != MediumRange {
			t.Errorf("run = %+v", r)
		}
	}
	if fmt.Sprint(got) != "[mean member1 member2]" {
		t.Errorf("members = %v", got)
	}
}

func TestEmptySeries(t *testing.T) {
	f := newFake(t, standard)
	res, err := client(f, Options{Series: []Series{Analysis, MediumRange, LongRange}}).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Series{MediumRange, LongRange} {
		runs, ok := res.Runs[s]
		if !ok || runs == nil || len(runs) != 0 {
			t.Errorf("%s = %#v, want empty non-nil slice", s, runs)
		}
		if res.Run(s) != nil {
			t.Errorf("Run(%s) not nil", s)
		}
	}
}

func TestPartialFailure(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		if s == "short_range" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		standard(w, s, n)
	})
	res, err := client(f, Options{}).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	if res.Run(Analysis) == nil {
		t.Error("analysis missing")
	}
	var e *Error
	if err := res.Errors[ShortRange]; !errors.As(err, &e) || e.Status != 500 || e.Op != "short_range" {
		t.Errorf("Errors = %v", res.Errors)
	}
	if _, ok := res.Runs[ShortRange]; ok {
		t.Error("failed series has runs")
	}
}

func TestAllFail(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	res, err := client(f, Options{}).Flow(ctx, 6495864)
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !res.AllFailed() {
		t.Error("AllFailed = false")
	}
	if len(res.Errors) != 2 || len(res.Runs) != 0 {
		t.Fatalf("Errors = %v, Runs = %v", res.Errors, res.Runs)
	}
	for _, s := range DefaultSeries() {
		var e *Error
		if err := res.Errors[s]; !errors.As(err, &e) || e.Status != 500 || e.Op != string(s) {
			t.Errorf("%s: err = %v", s, err)
		}
	}
	if res.Reach.ID != 6495864 || res.Reach.Name != "" || res.Reach.Lat != nil || res.Reach.Lon != nil {
		t.Errorf("Reach = %+v", res.Reach)
	}
	if res.Run(Analysis) != nil {
		t.Error("Run on failed series")
	}
}

func TestPartialNotAllFailed(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		if s == "short_range" {
			http.Error(w, "boom", 500)
			return
		}
		standard(w, s, n)
	})
	res, err := client(f, Options{}).Flow(ctx, 6495864)
	if err != nil || res.AllFailed() || len(res.Errors) != 1 || res.Run(Analysis) == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Reach.Name != "Amicalola Creek" {
		t.Errorf("Reach = %+v", res.Reach)
	}
}

func TestContextAlreadyDone(t *testing.T) {
	f := newFake(t, standard)
	c, cancel := context.WithCancel(ctx)
	cancel()
	res, err := client(f, Options{}).Flow(c, 6495864)
	var e *Error
	if res != nil || !errors.As(err, &e) || e.Op != "flow" || !errors.Is(err, context.Canceled) {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if f.total() != 0 {
		t.Error("request made with done context")
	}
}

func TestContextEndsWhileFetching(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { time.Sleep(300 * time.Millisecond) })
	c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	res, err := client(f, Options{}).Flow(c, 6495864)
	var e *Error
	if res != nil || !errors.As(err, &e) || e.Op != "flow" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestLatLon(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		fmt.Fprint(w, `{"reach":{"reachId":"6495864","name":"No Place"},"shortRange":{}}`)
	})
	res, err := client(f, Options{Series: []Series{ShortRange}}).Flow(ctx, 6495864)
	if err != nil || res.Reach.Lat != nil || res.Reach.Lon != nil || res.Reach.Name != "No Place" {
		t.Fatalf("absent: res=%+v err=%v", res, err)
	}
	f = newFake(t, standard)
	res, err = client(f, Options{Series: []Series{ShortRange}}).Flow(ctx, 6495864)
	if err != nil || res.Reach.Lat == nil || res.Reach.Lon == nil || *res.Reach.Lat != 34.4119 || *res.Reach.Lon != -84.213 {
		t.Fatalf("present: res=%+v err=%v", res, err)
	}
}

func TestUnavailable(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { unavailableBody(w) })
	res, err := client(f, Options{}).Flow(ctx, 6495864)
	err = seriesErr(t, res, err, ShortRange)
	var e *Error
	if !errors.Is(err, ErrUnavailable) || !errors.As(err, &e) || e.Status != 503 {
		t.Fatalf("err = %v", err)
	}
	if f.count("short_range") != 2 {
		t.Errorf("requests = %d, want 1 retry", f.count("short_range"))
	}

	// A plain 503 isn't "Data unavailable".
	f = newFake(t, func(w http.ResponseWriter, s string, n int) { http.Error(w, "down", 503) })
	res, err = client(f, Options{}).Flow(ctx, 6495864)
	err = seriesErr(t, res, err, ShortRange)
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestRetry(t *testing.T) {
	for _, status := range []int{503, 504, 502} {
		f := newFake(t, func(w http.ResponseWriter, s string, n int) {
			if n == 1 {
				w.WriteHeader(status)
				return
			}
			standard(w, s, n)
		})
		res, err := client(f, Options{}).Flow(ctx, 6495864)
		if err != nil || res.Errors != nil || res.Run(Analysis) == nil {
			t.Errorf("%d: res=%+v err=%v", status, res, err)
		}
		if f.count("analysis_assimilation") != 2 {
			t.Errorf("%d: requests = %d", status, f.count("analysis_assimilation"))
		}
	}
}

func TestRetryOnce(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { w.WriteHeader(504) })
	res, err := client(f, Options{Series: []Series{Analysis}}).Flow(ctx, 6495864)
	err = seriesErr(t, res, err, Analysis)
	var e *Error
	if !errors.As(err, &e) || !e.Timeout() {
		t.Errorf("err = %v", err)
	}
	if f.total() != 2 {
		t.Errorf("requests = %d, want 2", f.total())
	}
}

func TestNoRetry(t *testing.T) {
	for _, status := range []int{404, 400} {
		f := newFake(t, func(w http.ResponseWriter, s string, n int) { w.WriteHeader(status) })
		res, err := client(f, Options{Series: []Series{Analysis}}).Flow(ctx, 6495864)
		seriesErr(t, res, err, Analysis)
		if f.total() != 1 {
			t.Errorf("%d: requests = %d", status, f.total())
		}
	}
}

func TestRetryRespectsContext(t *testing.T) {
	old := retryPause
	retryPause = time.Hour
	defer func() { retryPause = old }()
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { w.WriteHeader(503) })
	c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if res, err := client(f, Options{Series: []Series{Analysis}}).Flow(c, 6495864); err == nil || res != nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("retry pause ignored the context")
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { time.Sleep(200 * time.Millisecond) })
	c := client(f, Options{Series: []Series{Analysis}, HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}})
	res, err := c.Flow(ctx, 6495864)
	err = seriesErr(t, res, err, Analysis)
	var e *Error
	if !errors.As(err, &e) || !e.Timeout() || e.Status != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestBadBody(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) { fmt.Fprint(w, "not json") })
	res, err := client(f, Options{Series: []Series{Analysis}}).Flow(ctx, 6495864)
	err = seriesErr(t, res, err, Analysis)
	if !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("err = %v", err)
	}
}

func TestInvalidCOMID(t *testing.T) {
	f := newFake(t, standard)
	for _, id := range []int{0, -5} {
		_, err := client(f, Options{}).Flow(ctx, id)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("comid %d: err = %v", id, err)
		}
	}
	if f.total() != 0 {
		t.Error("request made for invalid COMID")
	}
}

func TestDefaultSeries(t *testing.T) {
	ds := DefaultSeries()
	ds[0] = "mutated"
	if DefaultSeries()[0] != Analysis {
		t.Error("DefaultSeries shares state")
	}
	opts := &Options{Series: []Series{LongRange, LongRange, ""}}
	c := NewClient(opts)
	if len(c.series) != 1 || c.series[0] != LongRange {
		t.Errorf("series = %v", c.series)
	}
	opts.Series[0] = Analysis
	if c.series[0] != LongRange {
		t.Error("client shares the caller's slice")
	}
}

func TestUnknownSeries(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		if s == "bogus" {
			http.Error(w, "bad series", 400)
			return
		}
		standard(w, s, n)
	})
	res, err := client(f, Options{Series: []Series{Analysis, "bogus"}}).Flow(ctx, 6495864)
	if err != nil || res.Errors["bogus"] == nil || res.Run(Analysis) == nil {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestNormalizeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"ft³/s": "ft^3/s", "ft3/s": "ft^3/s", "ft^3/s": "ft^3/s", "cfs": "ft^3/s", "CFS": "ft^3/s",
		" ft³/s ": "ft^3/s", "m³/s": "m³/s", "": "",
	} {
		if got := normalizeUnit(in); got != want {
			t.Errorf("normalizeUnit(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeStore is a Store with a settable clock and optional failures.
type fakeStore struct {
	mu       sync.Mutex
	now      time.Time
	items    map[string]fakeItem
	failGet  bool
	failSet  bool
	sets     int
	lastKeys []string
}

type fakeItem struct {
	val []byte
	exp time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), items: map[string]fakeItem{}}
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet {
		return nil, false, errors.New("store down")
	}
	it, ok := s.items[key]
	if !ok || !s.now.Before(it.exp) {
		return nil, false, nil
	}
	return it.val, true, nil
}

func (s *fakeStore) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSet {
		return errors.New("store down")
	}
	s.sets++
	s.lastKeys = append(s.lastKeys, key)
	s.items[key] = fakeItem{val: val, exp: s.now.Add(ttl)}
	return nil
}

func (s *fakeStore) advance(d time.Duration) { s.mu.Lock(); s.now = s.now.Add(d); s.mu.Unlock() }

func TestCacheHit(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	c := client(f, Options{Cache: st, Series: []Series{Analysis, ShortRange, MediumRange}})
	first, err := c.Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	if f.total() != 3 {
		t.Fatalf("requests = %d", f.total())
	}
	second, err := c.Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	if f.total() != 3 {
		t.Errorf("requests after cached call = %d, want 3", f.total())
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("cached result differs:\n%v\n%v", first, second)
	}
	if r := second.Run(Analysis); r == nil || r.Unit != "ft^3/s" || len(r.Points) != 2 {
		t.Errorf("cached analysis = %+v", r)
	}
	// Empty medium range is cached too, and stays an empty non-nil slice.
	if runs, ok := second.Runs[MediumRange]; !ok || runs == nil || len(runs) != 0 {
		t.Errorf("cached empty series = %#v", runs)
	}
	if second.Reach.Name != "Amicalola Creek" {
		t.Errorf("Reach = %+v", second.Reach)
	}
	want := "modelflow:v3:" + f.srv.URL + ":6495864:analysis_assimilation"
	if _, ok := st.items[want]; !ok {
		t.Errorf("key %q not set; keys %v", want, st.lastKeys)
	}
}

func TestCacheFetchesOnlyMissing(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	client(f, Options{Cache: st, Series: []Series{Analysis}}).Flow(ctx, 6495864)
	c := client(f, Options{Cache: st})
	if _, err := c.Flow(ctx, 6495864); err != nil {
		t.Fatal(err)
	}
	if f.count("analysis_assimilation") != 1 || f.count("short_range") != 1 {
		t.Errorf("counts: analysis %d, short %d", f.count("analysis_assimilation"), f.count("short_range"))
	}
}

func TestCacheExpiry(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	c := client(f, Options{Cache: st, Series: []Series{Analysis}})
	c.Flow(ctx, 6495864)
	st.advance(14 * time.Minute)
	c.Flow(ctx, 6495864)
	if f.total() != 1 {
		t.Fatalf("requests = %d before TTL", f.total())
	}
	st.advance(time.Minute)
	c.Flow(ctx, 6495864)
	if f.total() != 2 {
		t.Errorf("requests = %d after TTL, want 2", f.total())
	}
}

func TestCacheTTLOption(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	c := client(f, Options{Cache: st, CacheTTL: time.Minute, Series: []Series{Analysis}})
	c.Flow(ctx, 6495864)
	st.advance(time.Minute)
	c.Flow(ctx, 6495864)
	if f.total() != 2 {
		t.Errorf("requests = %d, want 2", f.total())
	}
}

func TestCacheNone(t *testing.T) {
	f := newFake(t, standard)
	c := client(f, Options{Cache: cache.None})
	c.Flow(ctx, 6495864)
	c.Flow(ctx, 6495864)
	if f.total() != 4 {
		t.Errorf("requests = %d, want 4", f.total())
	}
}

func TestDefaultCacheIsMemory(t *testing.T) {
	f := newFake(t, standard)
	c := client(f, Options{})
	c.Flow(ctx, 6495864)
	c.Flow(ctx, 6495864)
	if f.total() != 2 {
		t.Errorf("requests = %d, want 2", f.total())
	}
}

func TestCacheDoesNotStoreFailures(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		if s == "short_range" && n == 1 { // 500 isn't retried
			http.Error(w, "boom", 500)
			return
		}
		standard(w, s, n)
	})
	st := newFakeStore()
	c := client(f, Options{Cache: st})
	res, _ := c.Flow(ctx, 6495864)
	if res.Errors[ShortRange] == nil {
		t.Fatal("expected short range failure")
	}
	if _, ok := st.items["modelflow:v3:"+f.srv.URL+":6495864:short_range"]; ok {
		t.Error("failure cached")
	}
	res, err := c.Flow(ctx, 6495864)
	if err != nil || res.Errors != nil {
		t.Errorf("second call: %+v %v", res, err)
	}
}

func TestFailingStore(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	st.failGet, st.failSet = true, true
	res, err := client(f, Options{Cache: st}).Flow(ctx, 6495864)
	if err != nil || res.Errors != nil || res.Run(Analysis) == nil {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestCorruptCacheEntry(t *testing.T) {
	f := newFake(t, standard)
	st := newFakeStore()
	st.Set(ctx, "modelflow:v3:"+f.srv.URL+":6495864:analysis_assimilation", []byte("garbage"), time.Hour)
	res, err := client(f, Options{Cache: st, Series: []Series{Analysis}}).Flow(ctx, 6495864)
	if err != nil || res.Run(Analysis) == nil || f.total() != 1 {
		t.Errorf("res=%+v err=%v requests=%d", res, err, f.total())
	}
}

func TestConcurrent(t *testing.T) {
	f := newFake(t, standard)
	c := client(f, Options{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Flow(ctx, 6495864); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestEmptyForms(t *testing.T) {
	for _, obj := range []string{`[]`, `null`, `{}`} {
		f := newFake(t, func(w http.ResponseWriter, s string, n int) { fmt.Fprint(w, body("mediumRange", obj)) })
		st := newFakeStore()
		res, err := client(f, Options{Cache: st, Series: []Series{MediumRange}}).Flow(ctx, 6495864)
		if err != nil {
			t.Fatalf("%s: %v", obj, err)
		}
		if runs, ok := res.Runs[MediumRange]; !ok || runs == nil || len(runs) != 0 {
			t.Errorf("%s: runs = %#v", obj, runs)
		}
		if len(st.items) != 1 {
			t.Errorf("%s: not cached", obj)
		}
	}
}

func TestZonelessTimes(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, s string, n int) {
		fmt.Fprint(w, body("shortRange", `{"series":`+member("2026-11-04T14:00:00", "cfs", pt("2026-11-04T15:00:00", "5"), pt("2026-11-04T16:00:00Z", "6"))+`}`))
	})
	res, err := client(f, Options{Series: []Series{ShortRange}}).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Run(ShortRange)
	if want := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC); !r.ReferenceTime.Equal(want) || len(r.Points) != 2 ||
		!r.Points[0].Time.Equal(time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("run = %+v", r)
	}
}

func TestCacheKeyVersion(t *testing.T) {
	c := NewClient(&Options{BaseURL: "https://api.example.test/v1"})
	if got := c.cacheKey(5, Analysis); !strings.HasPrefix(got, "modelflow:v3:") {
		t.Errorf("key = %q", got)
	}
}

func TestCacheKeyHost(t *testing.T) {
	a := NewClient(&Options{BaseURL: "https://API.example.test/v1"})
	b := NewClient(&Options{BaseURL: "https://other.example.test/v1"})
	if got := a.cacheKey(5, ShortRange); got != "modelflow:v3:https://api.example.test:5:short_range" {
		t.Errorf("key = %q", got)
	}
	if a.cacheKey(5, ShortRange) == b.cacheKey(5, ShortRange) {
		t.Error("keys collide across hosts")
	}
	h := NewClient(&Options{BaseURL: "http://api.example.test/v1"})
	if h.cacheKey(5, ShortRange) == a.cacheKey(5, ShortRange) {
		t.Error("keys collide across schemes")
	}
}

// TestLive hits the real NWPS API.
func TestLive(t *testing.T) {
	if os.Getenv("TRIBUTARY_LIVE") == "" {
		t.Skip("set TRIBUTARY_LIVE=1 to hit the real API")
	}
	res, err := NewClient(nil).Flow(ctx, 6495864)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != nil {
		t.Errorf("Errors = %v", res.Errors)
	}
	if res.Reach.Lat == nil || res.Reach.Lon == nil {
		t.Error("reach has no location")
	}
	t.Logf("reach %+v", res.Reach)
	for _, s := range DefaultSeries() {
		r := res.Run(s)
		if r == nil || len(r.Points) == 0 {
			t.Errorf("%s empty", s)
			continue
		}
		t.Logf("%s: %d points, ref %v, %s, first %+v last %+v", s, len(r.Points), r.ReferenceTime, r.Unit,
			r.Points[0], r.Points[len(r.Points)-1])
		if r.Unit != "ft^3/s" {
			t.Errorf("%s unit = %q", s, r.Unit)
		}
	}
}
