// Package modelflow fetches National Water Model (NWM) streamflow for a river
// reach: the model's estimate of recent flow and its short-range forecast.
//
// It uses the National Water Prediction Service (NWPS) API from NOAA. Reaches
// are NHDPlusV2 reaches identified by COMID, the same COMID the gauges package
// returns, so a reach need not have a gauge. The values are model output, not
// measurements; see docs/modelflow.md for how far to trust them.
package modelflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevenosloan/tributary-utils/cache"
	"github.com/stevenosloan/tributary-utils/internal/httpx"
)

// DefaultBaseURL is the NOAA National Water Prediction Service API.
const DefaultBaseURL = "https://api.water.noaa.gov/nwps/v1"

// DefaultCacheTTL is how long Flow keeps a series in the cache.
const DefaultCacheTTL = 15 * time.Minute

// maxConcurrent caps requests in flight at once for one Flow call.
const maxConcurrent = 4

// retryPause is how long Flow waits before retrying a failed request. It is a
// variable so tests can shorten it.
var retryPause = 500 * time.Millisecond

// unitCFS is the cubic feet per second unit, spelled as the readings package
// spells it.
const unitCFS = "ft^3/s"

// ErrUnavailable is wrapped by an *Error when NWPS answers "Data unavailable"
// (its code 14). NWPS gives this answer both for a reach ID it doesn't know
// and during an outage, so it can't tell the two apart; check the COMID, and
// try again later.
var ErrUnavailable = errors.New("modelflow: data unavailable (unknown reach or temporary outage)")

// Error reports a failed request to NWPS.
type Error struct {
	Op     string // the series requested, e.g. "short_range"; "flow" if no request was made
	Status int    // HTTP status; 0 if no response arrived
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("modelflow: %s: %v", e.Op, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

// Timeout reports whether the call timed out, either waiting on the API or
// with the API answering 504 itself.
func (e *Error) Timeout() bool { return httpx.Timeout(e.Status, e.Err) }

// Series names one of the forecast series NWPS serves.
type Series string

const (
	Analysis         Series = "analysis_assimilation" // recent model estimate, ~48h hourly
	ShortRange       Series = "short_range"           // next 18h hourly
	MediumRange      Series = "medium_range"          // days ahead; served empty so far
	MediumRangeBlend Series = "medium_range_blend"    // blended days-ahead; served empty so far
	LongRange        Series = "long_range"            // weeks ahead; served empty so far
)

// field is the name of the series' object in the response.
func (s Series) field() string {
	switch s {
	case Analysis:
		return "analysisAssimilation"
	case ShortRange:
		return "shortRange"
	case MediumRange:
		return "mediumRange"
	case MediumRangeBlend:
		return "mediumRangeBlend"
	case LongRange:
		return "longRange"
	}
	return ""
}

// DefaultSeries returns the series Flow fetches when Options.Series is empty:
// Analysis and ShortRange. Medium and long range are left out because NWPS
// has served them empty.
func DefaultSeries() []Series { return []Series{Analysis, ShortRange} }

// Options configures a Client.
type Options struct {
	BaseURL    string       // defaults to DefaultBaseURL
	HTTPClient *http.Client // defaults to a client with a 30s timeout
	// Series picks which series Flow fetches; nil or empty means
	// DefaultSeries. Medium and long range are opt-in because NWPS has
	// served them empty; enable them here when they're available. A value
	// that isn't one of the Series constants is sent to NWPS as given, and
	// NWPS's refusal is reported for that series in Result.Errors.
	Series []Series
	// Cache holds fetched series between calls. nil means a new in-memory
	// store; cache.None disables caching.
	Cache cache.Store
	// CacheTTL is how long a fetched series is kept; defaults to
	// DefaultCacheTTL.
	CacheTTL time.Duration
}

// Client queries NWPS. It is safe for concurrent use.
type Client struct {
	base   string
	hc     *http.Client
	series []Series
	cache  cache.Store
	ttl    time.Duration
}

// NewClient returns a Client; a nil opts uses defaults.
func NewClient(opts *Options) *Client {
	c := &Client{base: DefaultBaseURL, hc: &http.Client{Timeout: 30 * time.Second},
		series: DefaultSeries(), cache: cache.NewMemory(), ttl: DefaultCacheTTL}
	if opts != nil {
		if opts.BaseURL != "" {
			c.base = strings.TrimRight(opts.BaseURL, "/")
		}
		if opts.HTTPClient != nil {
			c.hc = opts.HTTPClient
		}
		var ss []Series
		for _, s := range opts.Series {
			if s != "" && !slices.Contains(ss, s) {
				ss = append(ss, s)
			}
		}
		if len(ss) > 0 {
			c.series = ss
		}
		if opts.Cache != nil {
			c.cache = opts.Cache
		}
		if opts.CacheTTL > 0 {
			c.ttl = opts.CacheTTL
		}
	}
	return c
}

// Reach identifies a river reach.
type Reach struct {
	ID       int // NHDPlusV2 COMID
	Name     string
	Lat, Lon float64
}

// Point is one flow value.
type Point struct {
	Time time.Time // the time the flow is for
	Flow float64
}

// Run is one model run of a series: for the analysis, the estimate of the
// last two days; for a forecast, the flows after ReferenceTime.
type Run struct {
	Series Series
	// Member names the ensemble member. Analysis and short range have one,
	// named "series". Medium and long range may have several.
	Member        string
	ReferenceTime time.Time // when the model run started
	Unit          string    // "ft^3/s" for the units NWPS has served
	Points        []Point   // sorted by time
}

// Result is the flow for one reach.
type Result struct {
	// Reach is from the first requested series that came back.
	Reach Reach
	// Runs holds the runs of each requested series that succeeded, members
	// sorted by name. A series that came back empty has an empty slice.
	Runs map[Series][]Run
	// Errors holds the requested series that failed; nil if none did.
	Errors map[Series]error
}

// Run returns the first run of s, or nil if s failed, wasn't requested or
// came back empty.
func (r *Result) Run(s Series) *Run {
	if runs := r.Runs[s]; len(runs) > 0 {
		return &runs[0]
	}
	return nil
}

// Flow returns the streamflow of the reach with the given COMID, fetching each
// configured series in parallel. A series that fails doesn't fail the others;
// it is recorded in Result.Errors. Flow itself returns an error only if every
// series failed, the first in request order. If every series was unavailable
// that error wraps ErrUnavailable. A comid <= 0 is an *Error without a
// request.
//
// Series found in the cache are not requested again. A request that fails with
// 502, 503 or 504, or times out, is retried once after a short pause.
func (c *Client) Flow(ctx context.Context, comid int) (*Result, error) {
	if comid <= 0 {
		return nil, &Error{Op: "flow", Err: fmt.Errorf("invalid COMID %d", comid)}
	}
	n := len(c.series)
	entries := make([]*entry, n)
	errs := make([]error, n)
	var fns []func(context.Context) error
	for i, s := range c.series {
		fns = append(fns, func(ctx context.Context) error {
			if e := c.cached(ctx, comid, s); e != nil {
				entries[i] = e
				return nil
			}
			e, err := c.fetch(ctx, comid, s)
			if err != nil {
				errs[i] = err
				return nil // the others carry on
			}
			entries[i] = e
			c.store(ctx, comid, s, e)
			return nil
		})
	}
	_ = httpx.Parallel(ctx, maxConcurrent, fns...)

	res := &Result{Runs: map[Series][]Run{}}
	var first error
	for i, s := range c.series {
		if errs[i] != nil {
			if res.Errors == nil {
				res.Errors = map[Series]error{}
			}
			res.Errors[s] = errs[i]
			if first == nil {
				first = errs[i]
			}
			continue
		}
		e := entries[i]
		if res.Reach.ID == 0 {
			res.Reach = e.Reach
		}
		res.Runs[s] = e.Runs
	}
	if len(res.Errors) == n {
		return nil, first
	}
	return res, nil
}

// entry is what is fetched, and cached, for one series of one reach.
type entry struct {
	Reach Reach
	Runs  []Run
}

// cacheKey includes the base URL's host, so clients on different endpoints
// can share a Store.
func (c *Client) cacheKey(comid int, s Series) string {
	host := c.base
	if u, err := url.Parse(c.base); err == nil && u.Host != "" {
		host = u.Host
	}
	return "modelflow:v1:" + strings.ToLower(host) + ":" + strconv.Itoa(comid) + ":" + string(s)
}

// cached returns the cached entry, or nil on a miss. A store error or an
// undecodable value is a miss.
func (c *Client) cached(ctx context.Context, comid int, s Series) *entry {
	b, ok, err := c.cache.Get(ctx, c.cacheKey(comid, s))
	if err != nil || !ok {
		return nil
	}
	var e entry
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	if e.Runs == nil {
		e.Runs = []Run{}
	}
	return &e
}

// store caches e, ignoring a store error.
func (c *Client) store(ctx context.Context, comid int, s Series, e *entry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = c.cache.Set(ctx, c.cacheKey(comid, s), b, c.ttl)
}

// fetch requests one series, retrying once if the failure may be passing.
func (c *Client) fetch(ctx context.Context, comid int, s Series) (*entry, error) {
	e, err := c.fetchOnce(ctx, comid, s)
	var ee *Error
	if err == nil || !errors.As(err, &ee) || !retryable(ee) {
		return e, err
	}
	t := time.NewTimer(retryPause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, err
	case <-t.C:
	}
	return c.fetchOnce(ctx, comid, s)
}

func retryable(e *Error) bool {
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.Timeout()
}

func (c *Client) fetchOnce(ctx context.Context, comid int, s Series) (*entry, error) {
	op := string(s)
	u := c.base + "/reaches/" + strconv.Itoa(comid) + "/streamflow?series=" + url.QueryEscape(op)
	status, body, err := httpx.Get(ctx, c.hc, u, nil)
	if err != nil {
		return nil, &Error{Op: op, Status: status, Err: err}
	}
	if status != http.StatusOK {
		err := httpx.StatusError(status, body)
		if unavailable(body) {
			err = fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return nil, &Error{Op: op, Status: status, Err: err}
	}
	e, err := decode(body, comid, s)
	if err != nil {
		return nil, &Error{Op: op, Status: status, Err: fmt.Errorf("decoding response: %w", err)}
	}
	return e, nil
}

// unavailable reports whether an error body is NWPS's "Data unavailable".
func unavailable(body []byte) bool {
	var b struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &b) != nil {
		return false
	}
	return b.Code == 14 || strings.EqualFold(strings.TrimSpace(b.Message), "Data unavailable")
}

type reachProps struct {
	ID   string   `json:"reachId"`
	Name string   `json:"name"`
	Lat  *float64 `json:"latitude"`
	Lon  *float64 `json:"longitude"`
}

type runProps struct {
	ReferenceTime string `json:"referenceTime"`
	Units         string `json:"units"`
	Data          []struct {
		ValidTime string   `json:"validTime"`
		Flow      *float64 `json:"flow"`
	} `json:"data"`
}

// decode reads the requested series out of a streamflow response. Only the
// object for the requested series is filled; the others are ignored.
func decode(body []byte, comid int, s Series) (*entry, error) {
	var top struct {
		Reach reachProps `json:"reach"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if r, ok := raw["reach"]; ok {
		if err := json.Unmarshal(r, &top.Reach); err != nil {
			return nil, fmt.Errorf("reach: %w", err)
		}
	}
	e := &entry{Reach: Reach{ID: comid, Name: top.Reach.Name}, Runs: []Run{}}
	if id, err := strconv.Atoi(top.Reach.ID); err == nil && id > 0 {
		e.Reach.ID = id
	}
	if top.Reach.Lat != nil && top.Reach.Lon != nil {
		e.Reach.Lat, e.Reach.Lon = *top.Reach.Lat, *top.Reach.Lon
	}

	f := s.field()
	if f == "" || raw[f] == nil {
		return e, nil
	}
	if t := strings.TrimSpace(string(raw[f])); t == "null" || t == "[]" {
		return e, nil
	}
	var members map[string]runProps
	if err := json.Unmarshal(raw[f], &members); err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := members[name]
		run := Run{Series: s, Member: name, Unit: normalizeUnit(m.Units)}
		run.ReferenceTime, _ = parseTime(m.ReferenceTime)
		for _, d := range m.Data {
			t, err := parseTime(d.ValidTime)
			if err != nil || d.Flow == nil || math.IsNaN(*d.Flow) || math.IsInf(*d.Flow, 0) {
				continue
			}
			run.Points = append(run.Points, Point{Time: t, Flow: *d.Flow})
		}
		if len(run.Points) == 0 {
			continue
		}
		sort.SliceStable(run.Points, func(i, j int) bool { return run.Points[i].Time.Before(run.Points[j].Time) })
		e.Runs = append(e.Runs, run)
	}
	return e, nil
}

// normalizeUnit spells cubic feet per second as "ft^3/s", as the readings
// package does, and leaves any other unit as given.
func normalizeUnit(u string) string {
	k := strings.ToLower(strings.TrimSpace(u))
	k = strings.NewReplacer("³", "3", "^", "", " ", "", "*", "").Replace(k)
	switch k {
	case "ft3/s", "cfs", "ft3s", "ft3/sec", "cuft/s":
		return unitCFS
	}
	return u
}

// parseTime reads an RFC 3339 time, or the same without a zone, as UTC.
func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		if t2, err2 := time.Parse("2006-01-02T15:04:05", v); err2 == nil {
			return t2, nil
		}
	}
	return t, err
}
