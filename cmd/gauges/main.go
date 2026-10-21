// Command gauges looks up USGS gauges near a lat/long for manual inspection.
//
//	go run ./cmd/gauges 40.0150 -105.2705
//	go run ./cmd/gauges -km 50 -json 40.0150 -105.2705
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/stevenosloan/tributary-utils/gauges"
	"github.com/stevenosloan/tributary-utils/gauges/render"
)

func main() {
	km := flag.Float64("km", gauges.DefaultDistanceKm, "max network distance in km")
	asJSON := flag.Bool("json", false, "output JSON")
	links := flag.Bool("links", false, "add a waterdata.usgs.gov link column")
	svgPath := flag.String("svg", "", "also write an SVG drawing of the result to this file")
	base := flag.String("base-url", "", "override NLDI base URL")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gauges [flags] <lat> <lon>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	lat, err1 := strconv.ParseFloat(flag.Arg(0), 64)
	lon, err2 := strconv.ParseFloat(flag.Arg(1), 64)
	if err1 != nil || err2 != nil {
		fmt.Fprintln(os.Stderr, "error: lat and lon must be numbers")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := gauges.NewClient(&gauges.Options{BaseURL: *base, Tributaries: *svgPath != ""}).Find(ctx, lat, lon, *km)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *svgPath != "" {
		if err := writeSVG(*svgPath, res); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "wrote", *svgPath)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	fmt.Printf("comid %d, snapped %.2f km from point, %d gauges within %.0f km\n\n",
		res.COMID, res.SnapKm, len(res.Gauges), *km)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "DIR\tSITE\tNET KM\tSTRAIGHT KM\tNAME"
	if *links {
		header += "\tLINK"
	}
	fmt.Fprintln(tw, header)
	for _, g := range res.Gauges {
		net := "-"
		if g.NetworkKm >= 0 {
			net = fmt.Sprintf("%.1f", g.NetworkKm)
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%.1f\t%s", g.Direction, g.SiteNo, net, g.StraightKm, g.Name)
		if *links {
			line += "\t" + g.URL
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
}

func writeSVG(path string, res *gauges.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := render.SVG(f, res, render.Options{}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
