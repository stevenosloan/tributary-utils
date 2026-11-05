// Package readings fetches the latest values reported by USGS stream gauges:
// discharge, gage height, water temperature and whatever else a gauge
// measures.
//
// It uses the USGS Water Data OGC API. Each gauge reports its own set of time
// series, one per parameter and sensor, so results list every series found
// rather than a fixed set of parameters.
package readings

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevenosloan/tributary-utils/internal/httpx"
)

// DefaultBaseURL is the USGS Water Data OGC API, which replaces the legacy
// waterservices.usgs.gov (NWIS) services.
const DefaultBaseURL = "https://api.waterdata.usgs.gov/ogcapi/v1"

// sentinel marks a missing value in USGS data.
const sentinel = -999999

// pageLimit is the page size requested; the API rejects more than 50000.
const pageLimit = 10000

// sitesPerRequest caps how many sites go in one request's query string, which
// otherwise outgrows the server's URL length limit for a large gauges.Find
// result.
const sitesPerRequest = 100

// maxConcurrent caps requests in flight at once for one Latest call.
const maxConcurrent = 4

// metaTTL is how long a gauge's series metadata is cached.
const metaTTL = 24 * time.Hour

// Error reports a failed request to the Water Data API.
type Error struct {
	Op     string // the collection queried, e.g. "latest-continuous"
	Status int    // HTTP status; 0 if no response arrived
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("readings: %s: %v", e.Op, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

// Timeout reports whether the call timed out, either waiting on the API or
// with the API answering 504 itself.
func (e *Error) Timeout() bool { return httpx.Timeout(e.Status, e.Err) }

// Reading is the latest value of one time series at a gauge.
type Reading struct {
	SeriesID      string // time series identifier
	ParameterCode string // e.g. "00060" (discharge)
	Parameter     string // e.g. "Discharge"; "" if the series has no metadata
	// Sublocation tells apart sensors measuring the same parameter, e.g.
	// "From multiparameter sonde". "" if not given.
	Sublocation string
	Unit        string // e.g. "ft^3/s", "degC"
	Value       float64
	Time        time.Time
	Approval    string   // "Provisional" or "Approved"
	Qualifiers  []string // e.g. "ICE", "EQUIP"
	// Discontinued is true for a series the gauge no longer reports. Its
	// value is the last one recorded, possibly years old.
	Discontinued bool
}

// Options configures a Client.
type Options struct {
	BaseURL    string       // defaults to DefaultBaseURL
	HTTPClient *http.Client // defaults to a client with a 30s timeout
	// APIKey is an optional api.data.gov key; requests without one get lower
	// rate limits.
	APIKey string
}

// Client queries the Water Data API. It is safe for concurrent use.
type Client struct {
	base string
	hc   *http.Client
	key  string

	now  func() time.Time
	mu   sync.Mutex
	meta map[string]siteMeta // keyed by monitoring location ID, "USGS-..."
}

// siteMeta is a gauge's cached series metadata, keyed by series ID.
type siteMeta struct {
	fetched time.Time
	series  map[string]metaProps
}

// NewClient returns a Client; a nil opts uses defaults.
func NewClient(opts *Options) *Client {
	c := &Client{base: DefaultBaseURL, hc: &http.Client{Timeout: 30 * time.Second},
		now: time.Now, meta: map[string]siteMeta{}}
	if opts != nil {
		if opts.BaseURL != "" {
			c.base = strings.TrimRight(opts.BaseURL, "/")
		}
		if opts.HTTPClient != nil {
			c.hc = opts.HTTPClient
		}
		c.key = opts.APIKey
	}
	return c
}

// Latest returns the most recent reading of every continuous time series at
// each gauge, keyed by site number (e.g. "02389400", as gauges.Gauge.SiteNo).
// A gauge with no series is missing from the map; that isn't an error.
// Readings with a missing value are left out. Each gauge's readings are
// sorted by parameter code, then active before discontinued, then
// sublocation.
//
// Series metadata (names, sublocations, status) is cached on the Client for
// 24 hours per gauge, and fetched again sooner if a gauge reports a series
// the cache doesn't know.
func (c *Client) Latest(ctx context.Context, siteNos []string) (map[string][]Reading, error) {
	out := map[string][]Reading{}
	if len(siteNos) == 0 {
		return out, nil
	}
	ids := make([]string, len(siteNos))
	for i, s := range siteNos {
		ids[i] = "USGS-" + strings.TrimPrefix(s, "USGS-")
	}

	// Values and metadata come from separate collections, joined on the
	// series ID. Metadata is only fetched for gauges not cached.
	known, stale := c.cachedMeta(ids)
	var (
		values = make([][]valueProps, len(chunk(ids)))
		meta   = make([][]metaProps, len(chunk(stale)))
		fns    []func(context.Context) error
	)
	for i, sites := range chunk(ids) {
		fns = append(fns, func(ctx context.Context) error {
			return fetch(ctx, c, "latest-continuous", url.Values{"monitoring_location_id": {sites}}, &values[i])
		})
	}
	for i, sites := range chunk(stale) {
		fns = append(fns, c.fetchMeta(sites, &meta[i]))
	}
	if err := httpx.Parallel(ctx, maxConcurrent, fns...); err != nil {
		return nil, err
	}

	// A cached gauge reporting a series the cache doesn't know has a new
	// series; refetch its metadata.
	var recheck []string
	for _, vs := range values {
		for _, v := range vs {
			if m, ok := known[v.Site]; ok && !slices.Contains(recheck, v.Site) {
				if _, ok := m.series[v.SeriesID]; !ok {
					recheck = append(recheck, v.Site)
				}
			}
		}
	}
	if len(recheck) > 0 {
		more := make([][]metaProps, len(chunk(recheck)))
		fns = fns[:0]
		for i, sites := range chunk(recheck) {
			fns = append(fns, c.fetchMeta(sites, &more[i]))
		}
		if err := httpx.Parallel(ctx, maxConcurrent, fns...); err != nil {
			return nil, err
		}
		meta = append(meta, more...)
	}
	for site, m := range c.storeMeta(append(stale, recheck...), meta, values) {
		known[site] = m
	}

	for _, vs := range values {
		for _, v := range vs {
			r, ok := v.reading(known[v.Site].series[v.SeriesID])
			if !ok {
				continue
			}
			site := strings.TrimPrefix(v.Site, "USGS-")
			out[site] = append(out[site], r)
		}
	}
	for _, rs := range out {
		sort.SliceStable(rs, func(i, j int) bool {
			a, b := rs[i], rs[j]
			if a.ParameterCode != b.ParameterCode {
				return a.ParameterCode < b.ParameterCode
			}
			if a.Discontinued != b.Discontinued {
				return !a.Discontinued
			}
			return a.Sublocation < b.Sublocation
		})
	}
	return out, nil
}

// Value is one past value of a gauge's time series.
type Value struct {
	Time       time.Time
	Value      float64
	Unit       string   // e.g. "ft^3/s"
	Approval   string   // "Provisional" or "Approved"
	Qualifiers []string // e.g. "ICE", "EQUIP"
	SeriesID   string   // time series ID, to tell sensors apart
}

// History returns the continuous (e.g. 15-minute) values a gauge reported for
// one parameter (e.g. "00060", discharge) between start and end, sorted by
// time. siteNo is as for Latest. A gauge with no values in the window gives an
// empty result, not an error. Values that are missing, NaN or infinite, or
// equal to the -999999 sentinel are left out.
//
// If the gauge has several series for the parameter (multiple sensors), the
// values of all of them are returned, each with its SeriesID; filter on it to
// pick one. An empty site or parameter code, or a start that isn't before
// end (compared to the second, as the request is), is an *Error without a
// request.
func (c *Client) History(ctx context.Context, siteNo, parameterCode string, start, end time.Time) ([]Value, error) {
	const op = "continuous"
	site := strings.TrimPrefix(strings.TrimSpace(siteNo), "USGS-")
	code := strings.TrimSpace(parameterCode)
	switch {
	case site == "":
		return nil, &Error{Op: op, Err: fmt.Errorf("empty site number")}
	case code == "":
		return nil, &Error{Op: op, Err: fmt.Errorf("empty parameter code")}
	case !start.Truncate(time.Second).Before(end.Truncate(time.Second)):
		return nil, &Error{Op: op, Err: fmt.Errorf("start %s is not before end %s", start.Format(time.RFC3339), end.Format(time.RFC3339))}
	}
	q := url.Values{
		"monitoring_location_id": {"USGS-" + site},
		"parameter_code":         {code},
		"time":                   {start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)},
	}
	var props []valueProps
	if err := fetch(ctx, c, op, q, &props); err != nil {
		return nil, err
	}
	out := []Value{}
	for _, v := range props {
		if r, ok := v.reading(metaProps{}); ok {
			out = append(out, Value{Time: r.Time, Value: r.Value, Unit: r.Unit,
				Approval: r.Approval, Qualifiers: r.Qualifiers, SeriesID: r.SeriesID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// chunk splits ids into comma-joined groups of at most sitesPerRequest.
func chunk(ids []string) []string {
	var out []string
	for len(ids) > 0 {
		n := min(len(ids), sitesPerRequest)
		out = append(out, strings.Join(ids[:n], ","))
		ids = ids[n:]
	}
	return out
}

func (c *Client) fetchMeta(sites string, dst *[]metaProps) func(context.Context) error {
	return func(ctx context.Context) error {
		q := url.Values{"monitoring_location_id": {sites}, "computation_period_identifier": {"Points"}}
		return fetch(ctx, c, "time-series-metadata", q, dst)
	}
}

// cachedMeta splits ids into gauges with fresh cached metadata and stale ones
// that need fetching.
func (c *Client) cachedMeta(ids []string) (known map[string]siteMeta, stale []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	known = map[string]siteMeta{}
	seen := map[string]bool{}
	now := c.now()
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if m, ok := c.meta[id]; ok && now.Sub(m.fetched) < metaTTL {
			known[id] = m
		} else {
			stale = append(stale, id)
		}
	}
	return known, stale
}

// storeMeta caches the metadata fetched for sites and returns it by site. A
// value whose series has no metadata gets an empty entry, so it isn't taken
// for a new series on the next call.
func (c *Client) storeMeta(sites []string, meta [][]metaProps, values [][]valueProps) map[string]siteMeta {
	now := c.now()
	got := make(map[string]siteMeta, len(sites))
	for _, s := range sites {
		got[s] = siteMeta{fetched: now, series: map[string]metaProps{}}
	}
	for _, ms := range meta {
		for _, m := range ms {
			if sm, ok := got[m.Site]; ok {
				sm.series[m.ID] = m
			}
		}
	}
	for _, vs := range values {
		for _, v := range vs {
			if sm, ok := got[v.Site]; ok {
				if _, ok := sm.series[v.SeriesID]; !ok {
					sm.series[v.SeriesID] = metaProps{ID: v.SeriesID, Site: v.Site}
				}
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for s, m := range got {
		c.meta[s] = m
	}
	return got
}

type valueProps struct {
	SeriesID   string   `json:"time_series_id"`
	Site       string   `json:"monitoring_location_id"`
	Code       string   `json:"parameter_code"`
	Time       string   `json:"time"`
	Value      *string  `json:"value"`
	Unit       string   `json:"unit_of_measure"`
	Approval   string   `json:"approval_status"`
	Qualifiers []string `json:"qualifier"`
}

type metaProps struct {
	ID          string  `json:"id"`
	Site        string  `json:"monitoring_location_id"`
	Name        string  `json:"parameter_name"`
	Sublocation *string `json:"sublocation_identifier"`
	Description *string `json:"web_description"`
}

// reading combines a latest value with its series metadata. ok is false when
// the value is missing or unusable.
func (v valueProps) reading(m metaProps) (r Reading, ok bool) {
	if v.Value == nil {
		return r, false
	}
	val, err := strconv.ParseFloat(*v.Value, 64)
	if err != nil || val == sentinel || math.IsNaN(val) || math.IsInf(val, 0) {
		return r, false
	}
	t, err := time.Parse(time.RFC3339, v.Time)
	if err != nil {
		return r, false
	}
	r = Reading{
		SeriesID: v.SeriesID, ParameterCode: v.Code, Parameter: m.Name,
		Unit: v.Unit, Value: val, Time: t, Approval: v.Approval,
		Qualifiers: v.Qualifiers,
	}
	if m.Sublocation != nil {
		r.Sublocation = *m.Sublocation
	}
	// web_description is free text; USGS writes "Discontinued" but may add to it.
	r.Discontinued = m.Description != nil &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(*m.Description)), "discontinued")
	for _, q := range v.Qualifiers {
		if strings.EqualFold(q, "DISCONTINUED") {
			r.Discontinued = true
		}
	}
	return r, true
}

type page[T any] struct {
	Features []struct {
		Properties T `json:"properties"`
	} `json:"features"`
	Links []struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	} `json:"links"`
}

// fetch reads every page of a collection's items matching q into dst. It
// follows next links only on the base URL's scheme and host, since each
// request carries the API key, and stops at a link it has already followed.
func fetch[T any](ctx context.Context, c *Client, op string, q url.Values, dst *[]T) error {
	q.Set("f", "json")
	q.Set("limit", strconv.Itoa(pageLimit))
	u := c.base + "/collections/" + op + "/items?" + q.Encode()
	seen := map[string]bool{}
	for u != "" && !seen[u] {
		seen[u] = true
		var p page[T]
		if err := c.get(ctx, op, u, &p); err != nil {
			return err
		}
		for _, f := range p.Features {
			*dst = append(*dst, f.Properties)
		}
		u = ""
		for _, l := range p.Links {
			if l.Rel == "next" {
				u = l.Href
			}
		}
		if u != "" && !c.onBase(u) {
			return &Error{Op: op, Err: fmt.Errorf("next link %q is not on %s", httpx.Truncate(u, 200), c.base)}
		}
	}
	return nil
}

// onBase reports whether u has the base URL's scheme and host.
func (c *Client) onBase(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	b, err := url.Parse(c.base)
	return err == nil && pu.Scheme == b.Scheme && strings.EqualFold(pu.Host, b.Host)
}

func (c *Client) get(ctx context.Context, op, u string, dst any) error {
	var h http.Header
	if c.key != "" {
		h = http.Header{"X-Api-Key": {c.key}}
	}
	status, body, err := httpx.Get(ctx, c.hc, u, h)
	if err != nil {
		return &Error{Op: op, Status: status, Err: err}
	}
	if status != http.StatusOK {
		return &Error{Op: op, Status: status, Err: httpx.StatusError(status, body)}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return &Error{Op: op, Status: status, Err: fmt.Errorf("decoding response: %w", err)}
	}
	return nil
}
