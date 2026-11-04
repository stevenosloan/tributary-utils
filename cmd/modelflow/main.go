// Command modelflow prints National Water Model streamflow for a reach, for
// manual inspection.
//
//	go run ./cmd/modelflow 6495864
//	go run ./cmd/modelflow -series analysis_assimilation,short_range -json 6495864
//	go run ./cmd/modelflow -at 34.4119 -84.213
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stevenosloan/tributary-utils/gauges"
	"github.com/stevenosloan/tributary-utils/modelflow"
)

// edge is how many points to show at each end of a long run.
const edge = 3

func main() {
	asJSON := flag.Bool("json", false, "output JSON")
	at := flag.Bool("at", false, "take <lat> <lon> instead of a COMID, and snap the point to its reach")
	series := flag.String("series", "", "comma-separated series to fetch (default analysis_assimilation,short_range)")
	base := flag.String("base-url", "", "override NWPS base URL")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: modelflow [flags] <comid>\n       modelflow [flags] -at <lat> <lon>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var (
		comid      int
		lat, lon   float64
		snapKm     float64
		err1, err2 error
	)
	if *at {
		if flag.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		lat, err1 = strconv.ParseFloat(flag.Arg(0), 64)
		lon, err2 = strconv.ParseFloat(flag.Arg(1), 64)
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "error: lat and lon must be numbers")
			os.Exit(2)
		}
	} else {
		if flag.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		comid, err1 = strconv.Atoi(flag.Arg(0))
		if err1 != nil || comid <= 0 {
			fmt.Fprintln(os.Stderr, "error: comid must be a positive integer")
			os.Exit(2)
		}
	}

	opts := &modelflow.Options{BaseURL: *base}
	for _, s := range strings.Split(*series, ",") {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(opts.Series, modelflow.Series(s)) {
			opts.Series = append(opts.Series, modelflow.Series(s))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *at {
		snap, err := gauges.NewClient(nil).Snap(ctx, lat, lon)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		comid, snapKm = snap.COMID, snap.SnapKm
		fmt.Fprintf(os.Stderr, "snapped %.4f, %.4f to comid %d (%.2f km from point)\n", lat, lon, comid, snapKm)
	}

	res, err := modelflow.NewClient(opts).Flow(ctx, comid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *asJSON {
		errs := map[string]string{}
		for s, err := range res.Errors {
			errs[string(s)] = err.Error()
		}
		out := struct {
			Reach  modelflow.Reach
			Runs   map[modelflow.Series][]modelflow.Run
			Errors map[string]string `json:",omitempty"`
		}{res.Reach, res.Runs, errs}
		if len(errs) == 0 {
			out.Errors = nil
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	r := res.Reach
	where := "-"
	if r.Lat != 0 || r.Lon != 0 {
		where = fmt.Sprintf("%.4f, %.4f", r.Lat, r.Lon)
	}
	fmt.Printf("comid %d  %s  (%s)\n", r.ID, r.Name, where)
	for _, s := range seriesOrder(opts.Series) {
		if err, ok := res.Errors[s]; ok {
			fmt.Printf("\n%s: error: %v\n", s, err)
			continue
		}
		runs := res.Runs[s]
		if len(runs) == 0 {
			fmt.Printf("\n%s: no data\n", s)
			continue
		}
		for _, run := range runs {
			printRun(run)
		}
	}
}

// seriesOrder is the order series are printed in: as requested.
func seriesOrder(req []modelflow.Series) []modelflow.Series {
	if len(req) == 0 {
		return modelflow.DefaultSeries()
	}
	return req
}

func printRun(run modelflow.Run) {
	fmt.Printf("\n%s  member %s  reference %s  unit %s  %d points\n",
		run.Series, run.Member, run.ReferenceTime.Format(time.RFC3339), run.Unit, len(run.Points))
	pts := run.Points
	lo, hi := pts[0], pts[0]
	for _, p := range pts {
		if p.Flow < lo.Flow {
			lo = p
		}
		if p.Flow > hi.Flow {
			hi = p
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(p modelflow.Point) { fmt.Fprintf(tw, "%s\t%.2f\n", p.Time.Format(time.RFC3339), p.Flow) }
	fmt.Fprintln(tw, "TIME\tFLOW")
	if len(pts) <= 2*edge+1 {
		for _, p := range pts {
			row(p)
		}
	} else {
		for _, p := range pts[:edge] {
			row(p)
		}
		fmt.Fprintf(tw, "... %d more\t\n", len(pts)-2*edge)
		for _, p := range pts[len(pts)-edge:] {
			row(p)
		}
	}
	tw.Flush()
	fmt.Printf("min %.2f at %s, max %.2f at %s\n", lo.Flow, lo.Time.Format(time.RFC3339), hi.Flow, hi.Time.Format(time.RFC3339))
}
