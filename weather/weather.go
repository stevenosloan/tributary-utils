// Package weather fetches current conditions and forecasts for a point from
// the US National Weather Service (NWS) API.
//
// Current conditions come from the weather stations nearest the point, not
// from the point itself, so each Observation says which station it is from and
// how far away that is. Forecasts are for the point's cell on the NWS grid.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stevenosloan/tributary-utils/internal/httpx"
)

// DefaultBaseURL is the NWS API.
const DefaultBaseURL = "https://api.weather.gov"

// maxStationKm is how far from the point a station may be to be considered.
const maxStationKm = 50

// maxStations caps how many stations Current returns.
const maxStations = 3

// maxAge is how old a station's newest observation may be before the station
// is skipped as not reporting.
const maxAge = 2 * time.Hour

// lookback is how much observation history is requested per station: enough
// for 24 hourly reports ending up to two hours ago.
const lookback = 26 * time.Hour

// batchSize is how many candidate stations are queried at once. A batch that
// doesn't yield maxStations fresh stations is followed by the next nearest.
const batchSize = 5

// maxConcurrent caps requests in flight at once for one call.
const maxConcurrent = 4

// stationLimit is how many stations are requested for a point's grid cell.
const stationLimit = 30

// pointTTL is how long a point's grid and station data is cached.
const pointTTL = 24 * time.Hour

// steadyHPa is the 3-hour pressure change below which the trend is "steady".
const steadyHPa = 1.0

// ErrNoCoverage is returned (wrapped in an *Error) for a point outside NWS
// coverage, such as open ocean or outside the US. Test with errors.Is.
var ErrNoCoverage = errors.New("point is outside NWS coverage")

// Error reports a failed request to the NWS API.
type Error struct {
	Op     string // the request, e.g. "points", "observations"
	Status int    // HTTP status; 0 if no response arrived
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("weather: %s: %v", e.Op, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

// Timeout reports whether the call timed out, either waiting on the API or
// with the API answering 504 itself.
func (e *Error) Timeout() bool { return httpx.Timeout(e.Status, e.Err) }

// Observation is the latest report from one weather station. Values the
// station doesn't report are nil, never 0.
type Observation struct {
	StationID   string
	StationName string
	Lat, Lon    float64  // station location
	DistanceKm  float64  // from the requested point, in a straight line
	ElevationM  *float64 // station elevation above sea level
	Time        time.Time
	Description string // e.g. "Mostly Cloudy"

	TemperatureC     *float64
	DewpointC        *float64
	HumidityPct      *float64
	WindSpeedKmh     *float64
	WindDirectionDeg *float64
	WindGustKmh      *float64
	// PressureHPa is station pressure, or sea level pressure if the station
	// doesn't report the former.
	PressureHPa *float64
	// PressureTrend is "rising", "falling" or "steady" over the last 3 hours,
	// or "" if unknown.
	PressureTrend     string
	PressureChangeHPa *float64 // change over the last 3 hours

	// Precipitation over the last 1, 6 and 24 hours. The 6 and 24 hour totals
	// sum the hours with a known value and are nil if there are none; the
	// MissingHours fields count the hours that were unknown, so a total with
	// missing hours may be low.
	PrecipLast1hMm            *float64
	PrecipLast6hMm            *float64
	PrecipLast24hMm           *float64
	PrecipLast6hMissingHours  int
	PrecipLast24hMissingHours int
}

// Forecast is the NWS forecast for a point.
type Forecast struct {
	Periods  []Period // 12-hour periods, about 7 days
	Hourly   []Period // the next 24 hours, hourly
	Updated  time.Time
	TimeZone string // IANA name, e.g. "America/New_York"
}

// Period is one forecast period.
type Period struct {
	Name            string // e.g. "Tonight"; often "" for hourly periods
	Start, End      time.Time
	IsDaytime       bool
	TemperatureC    *float64
	PrecipChancePct *float64
	WindSpeed       string // NWS text, e.g. "5 to 10 km/h"
	WindDirection   string // e.g. "NW"
	Summary         string // short forecast, e.g. "Sunny"
	Detail          string // detailed forecast; may be empty for hourly periods
}

// Options configures a Client.
type Options struct {
	// UserAgent identifies the application to NWS, which requires it, in the
	// form "(app name, contact)".
	UserAgent  string
	BaseURL    string       // defaults to DefaultBaseURL
	HTTPClient *http.Client // defaults to a client with a 30s timeout
}

// Client queries the NWS API. It is safe for concurrent use.
type Client struct {
	base string
	hc   *http.Client
	ua   string

	now   func() time.Time
	mu    sync.Mutex
	cache map[string]*pointEntry // keyed by lat,lon rounded to 4 decimals
}

// pointEntry is the cached grid and station data for a point.
type pointEntry struct {
	fetched  time.Time
	info     pointInfo
	stations []station // valid only when haveStns is set
	haveStns bool
}

type pointInfo struct {
	forecast, hourly, stationsURL string
	timeZone                      string
}

type station struct {
	id, name string
	lat, lon float64
	elevM    *float64
}

// NewClient returns a Client. Unlike readings.NewClient it takes a required
// argument and can fail: NWS asks every caller to identify itself in the
// User-Agent header and may block those who don't, so opts.UserAgent must be
// set.
func NewClient(opts *Options) (*Client, error) {
	if opts == nil || strings.TrimSpace(opts.UserAgent) == "" {
		return nil, errors.New("weather: Options.UserAgent is required, in the form \"(app name, contact)\"")
	}
	c := &Client{base: DefaultBaseURL, hc: &http.Client{Timeout: 30 * time.Second},
		ua: strings.TrimSpace(opts.UserAgent), now: time.Now, cache: map[string]*pointEntry{}}
	if opts.BaseURL != "" {
		c.base = strings.TrimRight(opts.BaseURL, "/")
	}
	if opts.HTTPClient != nil {
		c.hc = opts.HTTPClient
	}
	return c, nil
}

// Current returns the latest observation from up to three weather stations
// near the point, nearest first. Only stations within 50 km are considered,
// and a station is skipped unless its newest report is under two hours old.
// An empty result isn't an error. A point outside NWS coverage gives an
// *Error wrapping ErrNoCoverage.
//
// The nearest station often lacks some measurements (humidity, dewpoint,
// pressure), which is why several are returned; the caller can fill gaps from
// the next one, bearing in mind its distance and elevation.
//
// Candidates are queried nearest-first in batches of five, in parallel. If a
// batch yields fewer than three fresh stations the next batch is queried. A
// station whose request fails is skipped, unless no station could be read, in
// which case the first error is returned.
//
// The point's grid data and station list are cached on the Client for 24
// hours, so reuse one Client.
func (c *Client) Current(ctx context.Context, lat, lon float64) ([]Observation, error) {
	e, err := c.point(ctx, lat, lon)
	if err != nil {
		return nil, err
	}
	stations, err := c.stations(ctx, lat, lon, e)
	if err != nil {
		return nil, err
	}

	type cand struct {
		st station
		km float64
	}
	var cands []cand
	for _, s := range stations {
		if km := haversineKm(lat, lon, s.lat, s.lon); km <= maxStationKm {
			cands = append(cands, cand{s, km})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].km < cands[j].km })

	var (
		out      []Observation
		firstErr error
		nOK      int // stations whose request succeeded
	)
	for len(cands) > 0 && len(out) < maxStations {
		batch := cands[:min(batchSize, len(cands))]
		cands = cands[len(batch):]
		got := make([]*Observation, len(batch))
		errs := make([]error, len(batch))
		fns := make([]func(context.Context) error, len(batch))
		for i, b := range batch {
			fns[i] = func(ctx context.Context) error {
				reports, err := c.observations(ctx, b.st.id)
				if err != nil {
					errs[i] = err
					return nil
				}
				got[i] = summarize(b.st, b.km, reports, c.now())
				return nil
			}
		}
		// Per-station failures are collected rather than returned, so one bad
		// station doesn't cancel the rest.
		_ = httpx.Parallel(ctx, maxConcurrent, fns...)
		if err := ctx.Err(); err != nil {
			return nil, &Error{Op: "observations", Err: err}
		}
		for i := range batch {
			if errs[i] == nil {
				nOK++
			} else if firstErr == nil {
				firstErr = errs[i]
			}
			if got[i] != nil && len(out) < maxStations {
				out = append(out, *got[i])
			}
		}
	}
	if len(out) == 0 && nOK == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Forecast returns the 12-hour forecast periods for the point and the hourly
// forecast for the next 24 hours, with temperatures in degrees Celsius. The
// point's grid data is cached as for Current.
func (c *Client) Forecast(ctx context.Context, lat, lon float64) (*Forecast, error) {
	e, err := c.point(ctx, lat, lon)
	if err != nil {
		return nil, err
	}
	var daily, hourly forecastDoc
	err = httpx.Parallel(ctx, maxConcurrent,
		func(ctx context.Context) error {
			return c.getFollow(ctx, "forecast", withQuery(e.info.forecast, "units=si"), &daily)
		},
		func(ctx context.Context) error {
			return c.getFollow(ctx, "forecast-hourly", withQuery(e.info.hourly, "units=si"), &hourly)
		})
	if err != nil {
		return nil, err
	}

	now := c.now()
	f := &Forecast{TimeZone: e.info.timeZone}
	if t, err := time.Parse(time.RFC3339, daily.Properties.UpdateTime); err == nil {
		f.Updated = t
	}
	for _, p := range daily.Properties.Periods {
		if q, ok := p.period(); ok {
			f.Periods = append(f.Periods, q)
		}
	}
	for _, p := range hourly.Properties.Periods {
		// The period in progress comes first, so cap at 24 to stay within a day.
		if q, ok := p.period(); ok && q.End.After(now) && q.Start.Before(now.Add(24*time.Hour)) && len(f.Hourly) < 24 {
			f.Hourly = append(f.Hourly, q)
		}
	}
	return f, nil
}

type forecastDoc struct {
	Properties struct {
		UpdateTime string       `json:"updateTime"`
		Periods    []periodJSON `json:"periods"`
	} `json:"properties"`
}

type periodJSON struct {
	Name            string   `json:"name"`
	Start           string   `json:"startTime"`
	End             string   `json:"endTime"`
	IsDaytime       bool     `json:"isDaytime"`
	Temperature     *float64 `json:"temperature"`
	TemperatureUnit string   `json:"temperatureUnit"`
	PrecipChance    *measure `json:"probabilityOfPrecipitation"`
	WindSpeed       string   `json:"windSpeed"`
	WindDirection   string   `json:"windDirection"`
	Short           string   `json:"shortForecast"`
	Detailed        string   `json:"detailedForecast"`
}

func (p periodJSON) period() (Period, bool) {
	start, err1 := time.Parse(time.RFC3339, p.Start)
	end, err2 := time.Parse(time.RFC3339, p.End)
	if err1 != nil || err2 != nil {
		return Period{}, false
	}
	q := Period{Name: p.Name, Start: start, End: end, IsDaytime: p.IsDaytime,
		WindSpeed: p.WindSpeed, WindDirection: p.WindDirection,
		Summary: p.Short, Detail: p.Detailed}
	if p.Temperature != nil {
		t := *p.Temperature
		if strings.EqualFold(p.TemperatureUnit, "F") {
			t = (t - 32) * 5 / 9
		}
		q.TemperatureC = &t
	}
	q.PrecipChancePct = p.PrecipChance.convert(unitPercent)
	return q, true
}

// withQuery appends a query string to u.
func withQuery(u, q string) string {
	if strings.Contains(u, "?") {
		return u + "&" + q
	}
	return u + "?" + q
}

// point returns the cached grid data for a point, fetching it if missing or
// older than 24 hours.
func (c *Client) point(ctx context.Context, lat, lon float64) (pointEntry, error) {
	if math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return pointEntry{}, &Error{Op: "points", Err: fmt.Errorf("invalid coordinates %v, %v", lat, lon)}
	}
	key := pointKey(lat, lon)
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && c.now().Sub(e.fetched) < pointTTL {
		defer c.mu.Unlock()
		return *e, nil
	}
	c.mu.Unlock()

	var doc struct {
		Properties struct {
			Forecast            string `json:"forecast"`
			ForecastHourly      string `json:"forecastHourly"`
			ObservationStations string `json:"observationStations"`
			TimeZone            string `json:"timeZone"`
		} `json:"properties"`
	}
	if err := c.get(ctx, "points", c.base+"/points/"+key, &doc); err != nil {
		var ee *Error
		if errors.As(err, &ee) && ee.Status == http.StatusNotFound {
			ee.Err = fmt.Errorf("%w: %v", ErrNoCoverage, ee.Err)
		}
		return pointEntry{}, err
	}
	p := doc.Properties
	for _, l := range []struct{ name, u string }{{"forecast", p.Forecast}, {"forecastHourly", p.ForecastHourly}, {"observationStations", p.ObservationStations}} {
		if l.u == "" || !c.onBase(l.u) {
			return pointEntry{}, &Error{Op: "points", Err: fmt.Errorf("%s link %q is not on %s", l.name, httpx.Truncate(l.u, 200), c.base)}
		}
	}
	e := &pointEntry{fetched: c.now(), info: pointInfo{forecast: p.Forecast, hourly: p.ForecastHourly,
		stationsURL: p.ObservationStations, timeZone: p.TimeZone}}
	c.mu.Lock()
	for k, old := range c.cache { // drop expired entries so the cache stays bounded
		if e.fetched.Sub(old.fetched) >= pointTTL {
			delete(c.cache, k)
		}
	}
	c.cache[key] = e
	c.mu.Unlock()
	return *e, nil
}

func pointKey(lat, lon float64) string { return fmt.Sprintf("%.4f,%.4f", lat, lon) }

// stations returns the stations for a point's grid cell, from the cache entry
// if present.
func (c *Client) stations(ctx context.Context, lat, lon float64, e pointEntry) ([]station, error) {
	if e.haveStns {
		return e.stations, nil
	}
	var doc struct {
		Features []struct {
			Geometry struct {
				Coordinates []float64 `json:"coordinates"`
			} `json:"geometry"`
			Properties struct {
				ID        string   `json:"stationIdentifier"`
				Name      string   `json:"name"`
				Elevation *measure `json:"elevation"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := c.getFollow(ctx, "stations", withQuery(e.info.stationsURL, fmt.Sprintf("limit=%d", stationLimit)), &doc); err != nil {
		return nil, err
	}
	var out []station
	for _, f := range doc.Features {
		p := f.Properties
		if p.ID == "" || len(f.Geometry.Coordinates) < 2 {
			continue
		}
		out = append(out, station{id: p.ID, name: p.Name, lon: f.Geometry.Coordinates[0],
			lat: f.Geometry.Coordinates[1], elevM: p.Elevation.convert(unitMetres)})
	}
	c.mu.Lock()
	if cur, ok := c.cache[pointKey(lat, lon)]; ok && cur.fetched.Equal(e.fetched) {
		cur.stations, cur.haveStns = out, true
	}
	c.mu.Unlock()
	return out, nil
}

// getFollow fetches a URL taken from an NWS response, refusing one that isn't
// on the base URL's scheme and host.
func (c *Client) getFollow(ctx context.Context, op, u string, dst any) error {
	if !c.onBase(u) {
		return &Error{Op: op, Err: fmt.Errorf("link %q is not on %s", httpx.Truncate(u, 200), c.base)}
	}
	return c.get(ctx, op, u, dst)
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
	h := http.Header{"User-Agent": {c.ua}}
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

// measure is an NWS quantity: a value, possibly null, and its unit.
type measure struct {
	Value    *float64 `json:"value"`
	UnitCode string   `json:"unitCode"`
}

type unitKind int

const (
	unitTemp unitKind = iota
	unitSpeed
	unitPressure
	unitPrecip
	unitPercent
	unitDegrees
	unitMetres
)

// convert returns the value in the kind's standard unit (degC, km/h, hPa, mm,
// percent, degrees, metres), or nil if the value is null, not finite or in a
// unit not recognised. A nil measure is nil.
func (m *measure) convert(k unitKind) *float64 {
	if m == nil || m.Value == nil || math.IsNaN(*m.Value) || math.IsInf(*m.Value, 0) {
		return nil
	}
	unit := m.UnitCode
	if i := strings.LastIndex(unit, ":"); i >= 0 {
		unit = unit[i+1:]
	}
	v := *m.Value
	switch k {
	case unitTemp:
		switch unit {
		case "degC":
		case "degF":
			v = (v - 32) * 5 / 9
		default:
			return nil
		}
	case unitSpeed:
		switch unit {
		case "km_h-1":
		case "m_s-1":
			v *= 3.6
		default:
			return nil
		}
	case unitPressure:
		switch unit {
		case "Pa":
			v /= 100
		case "hPa":
		default:
			return nil
		}
	case unitPrecip:
		switch unit {
		case "mm":
		case "m":
			v *= 1000
		default:
			return nil
		}
	case unitPercent:
		if unit != "percent" {
			return nil
		}
	case unitDegrees:
		if unit != "degree_(angle)" {
			return nil
		}
	case unitMetres:
		if unit != "m" {
			return nil
		}
	}
	return &v
}

// obsProps is the part of an observation we read. A measure that is absent
// from the JSON is nil; one present with a null value has a nil Value.
type obsProps struct {
	Timestamp   string `json:"timestamp"`
	Description string `json:"textDescription"`
	RawMessage  string `json:"rawMessage"`

	Temperature   *measure `json:"temperature"`
	Dewpoint      *measure `json:"dewpoint"`
	Humidity      *measure `json:"relativeHumidity"`
	WindSpeed     *measure `json:"windSpeed"`
	WindDirection *measure `json:"windDirection"`
	WindGust      *measure `json:"windGust"`
	Barometric    *measure `json:"barometricPressure"`
	SeaLevel      *measure `json:"seaLevelPressure"`
	Precip1h      *measure `json:"precipitationLastHour"`
}

// report is a parsed observation.
type report struct {
	t time.Time
	p obsProps
}

// observations fetches a station's recent reports, newest first.
// 500 is the most NWS returns per request; it covers the 26 hour lookback even
// at a 5-minute cadence (about 312 reports), so pagination links are ignored.
func (c *Client) observations(ctx context.Context, id string) ([]report, error) {
	start := c.now().Add(-lookback).UTC().Format(time.RFC3339)
	u := c.base + "/stations/" + url.PathEscape(id) + "/observations?limit=500&start=" + url.QueryEscape(start)
	var doc struct {
		Features []struct {
			Properties obsProps `json:"properties"`
		} `json:"features"`
	}
	if err := c.get(ctx, "observations", u, &doc); err != nil {
		return nil, err
	}
	var rs []report
	for _, f := range doc.Features {
		if t, err := time.Parse(time.RFC3339, f.Properties.Timestamp); err == nil {
			rs = append(rs, report{t, f.Properties})
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].t.After(rs[j].t) })
	return rs, nil
}

// summarize builds the Observation for a station from its reports, newest
// first. It returns nil if there are none or the newest is older than maxAge.
func summarize(st station, km float64, rs []report, now time.Time) *Observation {
	if len(rs) == 0 || now.Sub(rs[0].t) > maxAge {
		return nil
	}
	p := rs[0].p
	o := &Observation{
		StationID: st.id, StationName: st.name, Lat: st.lat, Lon: st.lon,
		DistanceKm: km, ElevationM: st.elevM, Time: rs[0].t, Description: p.Description,
		TemperatureC:     p.Temperature.convert(unitTemp),
		DewpointC:        p.Dewpoint.convert(unitTemp),
		HumidityPct:      p.Humidity.convert(unitPercent),
		WindSpeedKmh:     p.WindSpeed.convert(unitSpeed),
		WindDirectionDeg: p.WindDirection.convert(unitDegrees),
		WindGustKmh:      p.WindGust.convert(unitSpeed),
	}
	// Station pressure when reported, else sea level pressure. The trend uses
	// the same measurement throughout.
	pick := func(p obsProps) *float64 { return p.Barometric.convert(unitPressure) }
	if pick(p) == nil {
		pick = func(p obsProps) *float64 { return p.SeaLevel.convert(unitPressure) }
	}
	o.PressureHPa = pick(p)
	o.PressureTrend, o.PressureChangeHPa = pressureTrend(rs, pick)
	pr := precipTotals(rs, now)
	o.PrecipLast1hMm, o.PrecipLast6hMm, o.PrecipLast24hMm = pr.h1, pr.h6, pr.h24
	o.PrecipLast6hMissingHours, o.PrecipLast24hMissingHours = pr.miss6, pr.miss24
	return o
}

// pressureTrend compares the newest report's pressure with that of the report
// closest to 3 hours earlier, within 30 minutes either way.
func pressureTrend(rs []report, pick func(obsProps) *float64) (string, *float64) {
	now := pick(rs[0].p)
	if now == nil {
		return "", nil
	}
	target := rs[0].t.Add(-3 * time.Hour)
	var (
		best     *float64
		bestDiff = 30*time.Minute + 1
	)
	for _, r := range rs[1:] {
		d := r.t.Sub(target)
		if d < 0 {
			d = -d
		}
		if v := pick(r.p); v != nil && d < bestDiff {
			best, bestDiff = v, d
		}
	}
	if best == nil {
		return "", nil
	}
	change := math.Round((*now-*best)*100) / 100
	switch {
	case math.Abs(change) < steadyHPa:
		return "steady", &change
	case change > 0:
		return "rising", &change
	}
	return "falling", &change
}

// precipitation holds the totals precipTotals works out.
type precipitation struct {
	h1, h6, h24   *float64
	miss6, miss24 int // hours in each window with no usable report
}

// precipTotals returns precipitation over the last 1, 6 and 24 hours as of the
// newest routine report.
//
// Stations file precipitationLastHour on their routine hourly report and on
// any special report, and a special's value overlaps the routine reports
// around it, so summing every report would double count. A routine report is
// taken to be one filed in the last 15 minutes of the hour (minute 45 to 59)
// that carries precipitationLastHour at all; the 5-minute reports many
// stations also file in that window omit it. When an hour has several, one
// with a raw message beats one without (a 5-minute report can carry the key as
// a null), and among equals the latest wins.
//
// An hour's value is the report's precipitationLastHour. A METAR omits its
// precipitation group when none fell, which NWS turns into a null, so a null
// counts as 0 mm only if the raw message shows an AO2 gauge (one that can tell
// precipitation from none) and no PNO (precipitation not available) flag.
// Any other null, a missing raw message, or an hour with no routine report is
// unknown.
//
// The 1 hour value is the newest routine report's hour. The 6 and 24 hour
// values sum the known hours of one routine report per clock hour, ending at
// that report's hour, and the missing counts say how many hours were unknown
// (the sum may then be low). With no known hour the total is nil. If the newest
// routine report is more than maxAge old, nothing is known.
func precipTotals(rs []report, now time.Time) precipitation {
	byHour := map[time.Time]*obsProps{}
	var anchor *report
	for i, r := range rs { // newest first, so the first per hour is the latest
		if r.t.UTC().Minute() < 45 || r.p.Precip1h == nil {
			continue
		}
		hr := r.t.UTC().Truncate(time.Hour)
		if old, ok := byHour[hr]; !ok || (old.RawMessage == "" && r.p.RawMessage != "") {
			byHour[hr] = &rs[i].p
		}
		if anchor == nil {
			anchor = &rs[i]
		}
	}
	if anchor == nil || now.Sub(anchor.t) > maxAge {
		return precipitation{miss6: 6, miss24: 24}
	}
	end := anchor.t.UTC().Truncate(time.Hour)
	sum := func(hours int) (*float64, int) {
		total, known := 0.0, 0
		for i := range hours {
			if p := byHour[end.Add(-time.Duration(i)*time.Hour)]; p != nil {
				if v := p.hourPrecip(); v != nil {
					total += *v
					known++
				}
			}
		}
		if known == 0 {
			return nil, hours
		}
		total = math.Round(total*100) / 100
		return &total, hours - known
	}
	var out precipitation
	out.h1 = byHour[end].hourPrecip()
	out.h6, out.miss6 = sum(6)
	out.h24, out.miss24 = sum(24)
	return out
}

// hourPrecip is a routine report's precipitation for its hour in mm, or nil if
// unknown.
func (p *obsProps) hourPrecip() *float64 {
	if p == nil {
		return nil
	}
	if v := p.Precip1h.convert(unitPrecip); v != nil {
		return v
	}
	// A null value: 0 only if the station would have reported any.
	if p.Precip1h.Value != nil { // a value in a unit we don't know
		return nil
	}
	ao2, pno := false, false
	for _, tok := range strings.Fields(p.RawMessage) {
		ao2 = ao2 || strings.HasPrefix(tok, "AO2")
		pno = pno || tok == "PNO"
	}
	if ao2 && !pno {
		zero := 0.0
		return &zero
	}
	return nil
}

// haversineKm is the great-circle distance between two points in kilometres.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthKm = 6371.0088
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKm * math.Asin(math.Min(1, math.Sqrt(a)))
}
