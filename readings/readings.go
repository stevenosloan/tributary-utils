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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the USGS Water Data OGC API, which replaces the legacy
// waterservices.usgs.gov (NWIS) services.
const DefaultBaseURL = "https://api.waterdata.usgs.gov/ogcapi/v1"

// sentinel marks a missing value in USGS data.
const sentinel = -999999

// pageLimit is the page size requested; the API rejects more than 50000.
const pageLimit = 10000

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
func (e *Error) Timeout() bool {
	var ne net.Error
	return e.Status == http.StatusGatewayTimeout ||
		errors.Is(e.Err, context.DeadlineExceeded) ||
		(errors.As(e.Err, &ne) && ne.Timeout())
}

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

// Client queries the Water Data API.
type Client struct {
	base string
	hc   *http.Client
	key  string
}

// NewClient returns a Client; a nil opts uses defaults.
func NewClient(opts *Options) *Client {
	c := &Client{base: DefaultBaseURL, hc: &http.Client{Timeout: 30 * time.Second}}
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
func (c *Client) Latest(ctx context.Context, siteNos []string) (map[string][]Reading, error) {
	out := map[string][]Reading{}
	if len(siteNos) == 0 {
		return out, nil
	}
	ids := make([]string, len(siteNos))
	for i, s := range siteNos {
		ids[i] = "USGS-" + strings.TrimPrefix(s, "USGS-")
	}
	sites := strings.Join(ids, ",")

	// Values and metadata (names, sublocations, status) come from separate
	// collections, joined on the series ID.
	var (
		values []valueProps
		meta   []metaProps
	)
	err := parallel(ctx,
		func(ctx context.Context) error {
			return fetch(ctx, c, "latest-continuous", url.Values{"monitoring_location_id": {sites}}, &values)
		},
		func(ctx context.Context) error {
			q := url.Values{"monitoring_location_id": {sites}, "computation_period_identifier": {"Points"}}
			return fetch(ctx, c, "time-series-metadata", q, &meta)
		},
	)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]metaProps, len(meta))
	for _, m := range meta {
		byID[m.ID] = m
	}
	for _, v := range values {
		r, ok := v.reading(byID[v.SeriesID])
		if !ok {
			continue
		}
		site := strings.TrimPrefix(v.Site, "USGS-")
		out[site] = append(out[site], r)
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
	if err != nil || val == sentinel {
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
	r.Discontinued = m.Description != nil && strings.EqualFold(*m.Description, "Discontinued")
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

// fetch reads every page of a collection's items matching q into dst.
func fetch[T any](ctx context.Context, c *Client, op string, q url.Values, dst *[]T) error {
	q.Set("f", "json")
	q.Set("limit", strconv.Itoa(pageLimit))
	u := c.base + "/collections/" + op + "/items?" + q.Encode()
	for u != "" {
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
	}
	return nil
}

func (c *Client) get(ctx context.Context, op, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Op: op, Status: resp.StatusCode,
			Err: fmt.Errorf("%s: %s", resp.Status, truncate(string(body), 200))}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("decoding response: %w", err)}
	}
	return nil
}

// parallel runs fns concurrently; the first error cancels the rest and is
// returned.
func parallel(ctx context.Context, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, fn := range fns {
		wg.Go(func() {
			if err := fn(ctx); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		})
	}
	wg.Wait()
	return first
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
