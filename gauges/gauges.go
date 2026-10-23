// Package gauges finds USGS stream gauges near a lat/long, tagged as upstream
// or downstream of the point along the NHDPlus river network.
//
// It uses the USGS Network-Linked Data Index (NLDI): the point is snapped to an
// NHDPlusV2 flowline (COMID), then the network is navigated up and downstream
// for indexed NWIS sites. Results are indexed to catchments, not exact points,
// so ordering within a single reach is approximate.
package gauges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the NLDI host (moved from labs.waterdata.usgs.gov in 2024).
const DefaultBaseURL = "https://api.water.usgs.gov/nldi/linked-data"

// DefaultDistanceKm caps network navigation; unbounded UT on big rivers is huge.
const DefaultDistanceKm = 25

// ErrNoCatchment is returned when the point isn't on the NHDPlus network
// (open ocean, outside the US, etc).
var ErrNoCatchment = errors.New("gauges: no catchment found at point")

// NLDIError reports a failed NLDI request: which call failed and why.
type NLDIError struct {
	Op     string // the call, e.g. "snap", "UT nwissite", "DM flowlines"
	Status int    // HTTP status; 0 if no response arrived
	Err    error
}

func (e *NLDIError) Error() string { return fmt.Sprintf("gauges: NLDI %s: %v", e.Op, e.Err) }

func (e *NLDIError) Unwrap() error { return e.Err }

// Timeout reports whether the call timed out, either waiting on NLDI or with
// NLDI answering 504 itself.
func (e *NLDIError) Timeout() bool {
	var ne net.Error
	return e.Status == http.StatusGatewayTimeout ||
		errors.Is(e.Err, context.DeadlineExceeded) ||
		(errors.As(e.Err, &ne) && ne.Timeout())
}

// Direction is a gauge's position relative to the query point.
type Direction string

const (
	Upstream   Direction = "upstream"
	Downstream Direction = "downstream"
)

// Gauge is a USGS monitoring location on the river network.
type Gauge struct {
	ID        string // full identifier, e.g. "USGS-06728000"
	SiteNo    string // ID without the "USGS-" prefix, for NWIS lookups
	Name      string
	Lat, Lon  float64
	COMID     int
	URL       string // monitoring-location page
	Direction Direction
	// Via lists the NLDI navigations that returned the gauge: "UT", "DD".
	Via []string
	// Mainstem is true if the gauge is on the mainstem path from the point.
	Mainstem bool
	// StraightKm is great-circle distance from the query point.
	StraightKm float64
	// NetworkKm is distance along the mainstem flowlines from the point. It is
	// -1 when the gauge is on a tributary (or a diversion) and not measurable
	// from the mainstem path.
	NetworkKm float64

	// measure is the NHDPlus percent along the reach: 0 at its downstream end,
	// 100 at its upstream end. nil if NLDI didn't supply one.
	measure *float64
}

// MonitoringLocationURL returns the waterdata.usgs.gov page for a gauge ID
// such as "USGS-06728000".
func MonitoringLocationURL(id string) string {
	return "https://waterdata.usgs.gov/monitoring-location/" + id
}

// Line is a polyline of [lon, lat] vertices.
type Line [][2]float64

// Result is the outcome of a Find call.
type Result struct {
	// Query echoes the inputs.
	Lat, Lon, DistanceKm float64

	COMID int
	// SnapKm is the distance from the query point to the snapped flowline, so
	// callers can sanity-check that the point landed on the intended stream.
	SnapKm float64
	// Gauges are sorted nearest-first by NetworkKm where known, then StraightKm.
	Gauges []Gauge

	// Geometry the result was computed from, for drawing. Each reach runs
	// upstream to downstream.
	Reach       Line   // the snapped start reach
	UpPath      []Line // mainstem reaches upstream of the point (UM)
	DownPath    []Line // mainstem reaches downstream of the point (DM)
	Tributaries []Line // all upstream reaches (UT); only if Options.Tributaries
}

// Options configures a Client.
type Options struct {
	BaseURL    string       // defaults to DefaultBaseURL
	HTTPClient *http.Client // defaults to a client with a 30s timeout
	// Tributaries also fetches every upstream reach (one extra request), for
	// drawing context. It does not change which gauges are returned.
	Tributaries bool
}

// Client queries the NLDI.
type Client struct {
	base  string
	hc    *http.Client
	tribs bool
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
		c.tribs = opts.Tributaries
	}
	return c
}

// Find returns gauges within distanceKm along the network of the point, tagged
// upstream or downstream. distanceKm <= 0 uses DefaultDistanceKm.
func (c *Client) Find(ctx context.Context, lat, lon, distanceKm float64) (*Result, error) {
	if err := checkCoords(lat, lon); err != nil {
		return nil, err
	}
	if distanceKm <= 0 {
		distanceKm = DefaultDistanceKm
	}

	start, err := c.snap(ctx, lat, lon)
	if err != nil {
		return nil, err
	}
	comid := start.reach.COMID

	// UT/DD catch tributary and diversion gauges; UM/DM flowlines give the
	// mainstem path used to measure network distance. The navigations are
	// independent, so run them together; the first failure cancels the rest.
	var (
		ut, dd                  []feature
		upLine, downLine, tribs []flowline
	)
	sites := func(dst *[]feature, mode string) func(context.Context) error {
		return func(ctx context.Context) (err error) {
			*dst, err = c.sites(ctx, comid, mode, distanceKm)
			return err
		}
	}
	flowlines := func(dst *[]flowline, mode string) func(context.Context) error {
		return func(ctx context.Context) (err error) {
			*dst, err = c.flowlines(ctx, comid, mode, distanceKm)
			return err
		}
	}
	navs := []func(context.Context) error{
		sites(&ut, "UT"), sites(&dd, "DD"),
		flowlines(&upLine, "UM"), flowlines(&downLine, "DM"),
	}
	if c.tribs {
		navs = append(navs, flowlines(&tribs, "UT"))
	}
	if err := parallel(ctx, navs...); err != nil {
		return nil, err
	}

	byID := map[string]*Gauge{}
	add := func(feats []feature, dir Direction, via string, line []flowline) {
		for _, f := range feats {
			g := f.gauge()
			if g.ID == "" {
				continue
			}
			if ex, ok := byID[g.ID]; ok {
				ex.Via = append(ex.Via, via) // start-reach gauges appear in both
				continue
			}
			g.Via = []string{via}
			g.StraightKm = haversineKm(lat, lon, g.Lat, g.Lon)
			if g.COMID == comid {
				// Same reach: NLDI can't say which side, so compare positions
				// along the reach.
				gp := gaugePosKm(start.reach, g)
				g.Direction, g.NetworkKm = Downstream, gp-start.posKm
				if gp < start.posKm {
					g.Direction, g.NetworkKm = Upstream, start.posKm-gp
				}
			} else {
				g.Direction = dir
				g.NetworkKm = networkKm(line, g, dir == Upstream, start)
			}
			g.Mainstem = g.NetworkKm >= 0
			byID[g.ID] = &g
		}
	}
	add(ut, Upstream, "UT", upLine)
	add(dd, Downstream, "DD", downLine)

	res := &Result{
		Lat: lat, Lon: lon, DistanceKm: distanceKm,
		COMID: comid, SnapKm: start.snapKm,
		Reach:       start.reach.Coords,
		UpPath:      lines(upLine),
		DownPath:    lines(downLine),
		Tributaries: lines(tribs),
	}
	for _, g := range byID {
		res.Gauges = append(res.Gauges, *g)
	}
	sort.Slice(res.Gauges, func(i, j int) bool {
		a, b := res.Gauges[i], res.Gauges[j]
		if (a.NetworkKm >= 0) != (b.NetworkKm >= 0) {
			return a.NetworkKm >= 0
		}
		if a.NetworkKm >= 0 && a.NetworkKm != b.NetworkKm {
			return a.NetworkKm < b.NetworkKm
		}
		if a.StraightKm != b.StraightKm {
			return a.StraightKm < b.StraightKm
		}
		return a.ID < b.ID
	})
	return res, nil
}

// --- NLDI plumbing ---

// parallel runs fns concurrently and returns the first error. That error
// cancels the others, so their resulting context errors are not reported.
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

type feature struct {
	Geometry struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
	Properties struct {
		Identifier string   `json:"identifier"`
		Name       string   `json:"name"`
		URI        string   `json:"uri"`
		COMID      int      `json:"comid"`
		NHDComid   int      `json:"nhdplus_comid"`
		Measure    *float64 `json:"measure"`
	} `json:"properties"`
}

type featureCollection struct {
	Features []feature `json:"features"`
}

func (f feature) gauge() Gauge {
	g := Gauge{
		ID:        f.Properties.Identifier,
		SiteNo:    strings.TrimPrefix(f.Properties.Identifier, "USGS-"),
		Name:      f.Properties.Name,
		COMID:     f.Properties.COMID,
		URL:       f.Properties.URI,
		NetworkKm: -1,
		measure:   f.Properties.Measure,
	}
	if g.URL == "" && g.ID != "" {
		g.URL = MonitoringLocationURL(g.ID)
	}
	var pt [2]float64
	if json.Unmarshal(f.Geometry.Coordinates, &pt) == nil {
		g.Lon, g.Lat = pt[0], pt[1]
	}
	return g
}

// get fetches an NLDI feature collection. op names the call in errors.
func (c *Client) get(ctx context.Context, op, path string, q url.Values) (*featureCollection, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &NLDIError{Op: op, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &NLDIError{Op: op, Status: resp.StatusCode, Err: err}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoCatchment
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &NLDIError{Op: op, Status: resp.StatusCode,
			Err: fmt.Errorf("%s: %s", resp.Status, truncate(string(body), 200))}
	}
	var fc featureCollection
	if err := json.Unmarshal(body, &fc); err != nil {
		return nil, &NLDIError{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("decoding response: %w", err)}
	}
	return &fc, nil
}

// Snap is a point placed on the river network.
type Snap struct {
	// Query echoes the inputs.
	Lat, Lon float64

	COMID int
	// SnapKm is the distance from the point to the reach. NLDI picks the reach
	// whose catchment contains the point, not the nearest stream, so a large
	// value means the point may be on a different stream than intended.
	SnapKm float64
	// Reach is the snapped reach, upstream to downstream.
	Reach Line
}

// Snap resolves a point to its NHDPlus reach without navigating the network,
// in one NLDI request. It fails as Find does: ErrNoCatchment off the network,
// an *NLDIError (Op "snap") when NLDI fails.
func (c *Client) Snap(ctx context.Context, lat, lon float64) (*Snap, error) {
	if err := checkCoords(lat, lon); err != nil {
		return nil, err
	}
	s, err := c.snap(ctx, lat, lon)
	if err != nil {
		return nil, err
	}
	return &Snap{Lat: lat, Lon: lon, COMID: s.reach.COMID, SnapKm: s.snapKm, Reach: s.reach.Coords}, nil
}

func checkCoords(lat, lon float64) error {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("gauges: invalid coordinates %v,%v", lat, lon)
	}
	return nil
}

// snapped is the query point placed on its flowline.
type snapped struct {
	reach  flowline
	snapKm float64 // distance from the point to the flowline
	posKm  float64 // distance from the reach's upstream end to the snapped point
}

// snap resolves a point to its flowline and its position along it.
func (c *Client) snap(ctx context.Context, lat, lon float64) (snapped, error) {
	// WKT order is lon lat.
	q := url.Values{"coords": {fmt.Sprintf("POINT(%v %v)", lon, lat)}}
	fc, err := c.get(ctx, "snap", "/comid/position", q)
	if err != nil {
		return snapped{}, err
	}
	if len(fc.Features) == 0 {
		return snapped{}, ErrNoCatchment
	}
	f := fc.Features[0]
	var coords [][2]float64
	_ = json.Unmarshal(f.Geometry.Coordinates, &coords)
	fl := newFlowline(f.Properties.COMID, coords)
	pos, dist := fl.project(lat, lon)
	return snapped{reach: fl, snapKm: dist, posKm: pos}, nil
}

func (c *Client) sites(ctx context.Context, comid int, mode string, km float64) ([]feature, error) {
	path := fmt.Sprintf("/comid/%d/navigation/%s/nwissite", comid, mode)
	fc, err := c.get(ctx, mode+" nwissite", path, url.Values{"distance": {fmt.Sprint(km)}})
	if err != nil {
		return nil, err
	}
	return fc.Features, nil
}

type flowline struct {
	COMID  int
	Coords [][2]float64 // lon, lat
	Km     float64
}

// flowlines returns the mainstem path in network order from the start reach.
func (c *Client) flowlines(ctx context.Context, comid int, mode string, km float64) ([]flowline, error) {
	path := fmt.Sprintf("/comid/%d/navigation/%s/flowlines", comid, mode)
	fc, err := c.get(ctx, mode+" flowlines", path, url.Values{"distance": {fmt.Sprint(km)}})
	if err != nil {
		return nil, err
	}
	out := make([]flowline, 0, len(fc.Features))
	for _, f := range fc.Features {
		var coords [][2]float64
		if err := json.Unmarshal(f.Geometry.Coordinates, &coords); err != nil {
			continue
		}
		fl := newFlowline(f.Properties.NHDComid, coords)
		out = append(out, fl)
	}
	return out, nil
}

func lines(fls []flowline) []Line {
	var out []Line
	for _, fl := range fls {
		out = append(out, fl.Coords)
	}
	return out
}

func newFlowline(comid int, coords [][2]float64) flowline {
	fl := flowline{COMID: comid, Coords: coords}
	for i := 1; i < len(coords); i++ {
		fl.Km += haversineKm(coords[i-1][1], coords[i-1][0], coords[i][1], coords[i][0])
	}
	return fl
}

// project returns the distance along the flowline from its first (upstream)
// vertex to the point's nearest position, and the distance to the flowline.
func (fl flowline) project(lat, lon float64) (alongKm, distKm float64) {
	distKm = math.Inf(1)
	var acc float64
	for i := 1; i < len(fl.Coords); i++ {
		a, b := fl.Coords[i-1], fl.Coords[i]
		segLen := haversineKm(a[1], a[0], b[1], b[0])
		d, t := segmentKm(lat, lon, a, b)
		if d < distKm {
			distKm, alongKm = d, acc+t*segLen
		}
		acc += segLen
	}
	if math.IsInf(distKm, 1) {
		distKm = 0
	}
	return alongKm, distKm
}

// gaugePosKm is the gauge's distance from the upstream end of its reach, from
// NLDI's measure when present, else by projecting its coordinates.
func gaugePosKm(fl flowline, g Gauge) float64 {
	if g.measure != nil {
		return (1 - *g.measure/100) * fl.Km
	}
	pos, _ := fl.project(g.Lat, g.Lon)
	return pos
}

// networkKm measures distance from the snapped point along the mainstem path
// to a gauge on a different reach: the rest of the start reach, the full
// reaches between, then the offset into the gauge's reach from its entry end.
// Returns -1 if the gauge's reach is not on the path.
func networkKm(line []flowline, g Gauge, upstream bool, start snapped) float64 {
	acc := start.reach.Km - start.posKm
	if upstream {
		acc = start.posKm
	}
	for _, fl := range line {
		switch fl.COMID {
		case start.reach.COMID:
		case g.COMID:
			// Flowlines run upstream->downstream: downstream travel enters a
			// reach at its upstream end, upstream travel at its downstream end.
			off := gaugePosKm(fl, g)
			if upstream {
				off = fl.Km - off
			}
			return acc + off
		default:
			acc += fl.Km
		}
	}
	return -1
}

// segmentKm is the distance from a point to the segment a-b (lon, lat pairs)
// and the fraction t along the segment of the nearest point,
// using a local flat-earth projection, which is fine at flowline scale.
func segmentKm(lat, lon float64, a, b [2]float64) (distKm, t float64) {
	const kmPerDeg = 111.195
	cos := math.Cos(lat * math.Pi / 180)
	px, py := (lon-a[0])*cos*kmPerDeg, (lat-a[1])*kmPerDeg
	bx, by := (b[0]-a[0])*cos*kmPerDeg, (b[1]-a[1])*kmPerDeg
	if l2 := bx*bx + by*by; l2 > 0 {
		t = math.Max(0, math.Min(1, (px*bx+py*by)/l2))
	}
	return math.Hypot(px-t*bx, py-t*by), t
}

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0088
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
