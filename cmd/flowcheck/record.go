package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stevenosloan/tributary-utils/cache"
	"github.com/stevenosloan/tributary-utils/gauges"
	"github.com/stevenosloan/tributary-utils/internal/httpx"
	"github.com/stevenosloan/tributary-utils/modelflow"
)

// recordConcurrency is how many sites record handles at once.
const recordConcurrency = 4

// refTimeLayout names forecast files after their reference time.
const refTimeLayout = "20060102T1504Z"

// forecastPoint is one forecast flow as saved.
type forecastPoint struct {
	Time time.Time `json:"time"`
	Flow float64   `json:"flow"`
}

// forecast is one saved short-range run for one gauge's reach.
type forecast struct {
	Site          string          `json:"site"`
	Label         string          `json:"label,omitempty"`
	COMID         int             `json:"comid"`
	Reach         string          `json:"reach_name"`
	Gauge         string          `json:"gauge_name"`
	ReferenceTime time.Time       `json:"reference_time"`
	Unit          string          `json:"unit"`
	Points        []forecastPoint `json:"points"`
}

func forecastPath(dir, siteNo string, ref time.Time) string {
	return filepath.Join(dir, "forecasts", siteNo, ref.UTC().Format(refTimeLayout)+".json")
}

type recordConfig struct {
	Sites   []site
	Dir     string
	NLDIURL string // defaults to gauges.DefaultBaseURL
	NWPSURL string // defaults to modelflow.DefaultBaseURL
	HTTP    *http.Client
}

// Outcomes of recording one site.
const (
	statusSaved = "saved"
	statusHad   = "already had"
)

type recordResult struct {
	Site   site
	Status string // statusSaved or statusHad; "" on error
	Detail string
	Err    error
}

// record fetches the current short-range forecast of each site and saves it
// under cfg.Dir, one line of status per site to out. It returns an error only
// if every site failed (or the cache couldn't be read).
func record(ctx context.Context, cfg recordConfig, out io.Writer) error {
	if cfg.NLDIURL == "" {
		cfg.NLDIURL = gauges.DefaultBaseURL
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	cacheFile, err := loadSiteCache(filepath.Join(cfg.Dir, "sites.json"))
	if err != nil {
		return err
	}
	mf := modelflow.NewClient(&modelflow.Options{
		BaseURL: cfg.NWPSURL, HTTPClient: hc,
		Series: []modelflow.Series{modelflow.ShortRange}, Cache: cache.None,
	})

	results := make([]recordResult, len(cfg.Sites))
	fns := make([]func(context.Context) error, len(cfg.Sites))
	for i, s := range cfg.Sites {
		fns[i] = func(ctx context.Context) error {
			results[i] = recordSite(ctx, cfg, hc, mf, cacheFile, s)
			return nil // one site failing doesn't stop the others
		}
	}
	_ = httpx.Parallel(ctx, recordConcurrency, fns...)

	var failed int
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(out, "%-9s error: %v\n", r.Site.No, r.Err)
			continue
		}
		fmt.Fprintf(out, "%-9s %s %s\n", r.Site.No, r.Status, r.Detail)
	}
	if err := cacheFile.save(); err != nil {
		fmt.Fprintf(out, "warning: saving sites.json: %v\n", err)
	}
	if len(cfg.Sites) > 0 && failed == len(cfg.Sites) {
		return errors.New("every site failed")
	}
	return nil
}

func recordSite(ctx context.Context, cfg recordConfig, hc *http.Client, mf *modelflow.Client, sc *siteCache, s site) recordResult {
	res := recordResult{Site: s}
	info, ok := sc.get(s.No)
	if !ok || info.COMID <= 0 {
		var err error
		if info, err = nldiLookup(ctx, hc, cfg.NLDIURL, s.No); err != nil {
			res.Err = err
			return res
		}
		sc.set(s.No, info)
	}
	flow, err := mf.Flow(ctx, info.COMID)
	if err != nil {
		res.Err = err
		return res
	}
	run := flow.Run(modelflow.ShortRange)
	if run == nil {
		if err := flow.Errors[modelflow.ShortRange]; err != nil {
			res.Err = err
			return res
		}
		res.Err = fmt.Errorf("no short-range forecast for comid %d", info.COMID)
		return res
	}
	if flow.Reach.Name != "" && info.Reach != flow.Reach.Name {
		info.Reach = flow.Reach.Name
		sc.set(s.No, info)
	}
	if run.ReferenceTime.IsZero() {
		res.Err = errors.New("forecast has no reference time")
		return res
	}
	res.Detail = run.ReferenceTime.UTC().Format(time.RFC3339)
	path := forecastPath(cfg.Dir, s.No, run.ReferenceTime)
	if _, err := os.Stat(path); err == nil {
		res.Status = statusHad
		return res
	}
	f := forecast{
		Site: s.No, Label: s.Label, COMID: info.COMID, Reach: info.Reach, Gauge: info.Gauge,
		ReferenceTime: run.ReferenceTime.UTC(), Unit: run.Unit,
	}
	for _, p := range run.Points {
		f.Points = append(f.Points, forecastPoint{Time: p.Time.UTC(), Flow: p.Flow})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, append(b, '\n'))
	}
	if err != nil {
		res.Err = err
		return res
	}
	res.Status = statusSaved
	res.Detail += fmt.Sprintf(" (%d points)", len(f.Points))
	return res
}
