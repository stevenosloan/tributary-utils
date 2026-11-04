// Command weather shows current conditions and the forecast near a lat/long
// for manual inspection. NWS requires a User-Agent identifying the caller, in
// the form "(app name, contact)", given with -ua or in NWS_USER_AGENT.
//
//	go run ./cmd/weather -ua "(example.com, contact@example.com)" 34.42 -84.22
//	go run ./cmd/weather -forecast -json 34.42 -84.22
//
// In the table, PRECIP shows the 1, 6 and 24 hour totals in mm as 1h/6h/24h.
// A 6 or 24 hour total with unknown hours is followed by "+Nh?", e.g.
// "3.0/3.0+2h?/3.0+20h?": the total may be low by whatever fell in those N
// hours. "-" means no hour was known.
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

	"github.com/stevenosloan/tributary-utils/weather"
)

func main() {
	ua := flag.String("ua", os.Getenv("NWS_USER_AGENT"), `User-Agent for NWS, "(app name, contact)" (default $NWS_USER_AGENT)`)
	asJSON := flag.Bool("json", false, "output JSON")
	showForecast := flag.Bool("forecast", false, "also fetch the forecast")
	base := flag.String("base-url", "", "override NWS base URL")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: weather [flags] <lat> <lon>\n\n")
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
	if *ua == "" {
		fmt.Fprintln(os.Stderr, "error: a User-Agent is required: pass -ua or set NWS_USER_AGENT")
		os.Exit(2)
	}

	c, err := weather.NewClient(&weather.Options{UserAgent: *ua, BaseURL: *base})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	obs, err := c.Current(ctx, lat, lon)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var fc *weather.Forecast
	if *showForecast {
		if fc, err = c.Forecast(ctx, lat, lon); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	if *asJSON {
		out := map[string]any{"observations": obs}
		if fc != nil {
			out["forecast"] = fc
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tKM\tELEV M\tAGE\tTEMP C\tWIND KM/H\tPRESSURE HPA\tPRECIP 1/6/24H MM\tHUMIDITY %\tDESCRIPTION")
	for _, o := range obs {
		wind := num(o.WindSpeedKmh, "%.0f")
		if o.WindDirectionDeg != nil && o.WindSpeedKmh != nil {
			wind += " @" + num(o.WindDirectionDeg, "%.0f")
		}
		if o.WindGustKmh != nil {
			wind += " g" + num(o.WindGustKmh, "%.0f")
		}
		pressure := num(o.PressureHPa, "%.1f")
		if o.PressureTrend != "" {
			pressure += " " + o.PressureTrend
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f\t%s\t%s\t%s\t%s\t%s\t%s/%s/%s\t%s\t%s\n",
			o.StationID, o.StationName, o.DistanceKm, num(o.ElevationM, "%.0f"),
			now.Sub(o.Time).Round(time.Minute), num(o.TemperatureC, "%.1f"), wind, pressure,
			num(o.PrecipLast1hMm, "%.1f"), precip(o.PrecipLast6hMm, o.PrecipLast6hMissingHours),
			precip(o.PrecipLast24hMm, o.PrecipLast24hMissingHours),
			num(o.HumidityPct, "%.0f"), o.Description)
	}
	tw.Flush()

	if fc == nil {
		return
	}
	fmt.Printf("\nforecast, updated %s, %s\n\n", fc.Updated.Format(time.RFC3339), fc.TimeZone)
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PERIOD\tSTART\tTEMP C\tPRECIP %\tWIND\tSUMMARY")
	for _, p := range fc.Periods {
		printPeriod(tw, p.Name, p)
	}
	for _, p := range fc.Hourly {
		printPeriod(tw, "hourly", p)
	}
	tw.Flush()
}

func printPeriod(tw *tabwriter.Writer, name string, p weather.Period) {
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s %s\t%s\n", name, p.Start.Format("Mon 02 15:04"),
		num(p.TemperatureC, "%.1f"), num(p.PrecipChancePct, "%.0f"), p.WindSpeed, p.WindDirection, p.Summary)
}

// num formats v, or "-" if it's nil.
func num(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

// precip formats a precipitation total, marking how many hours were unknown.
func precip(v *float64, missing int) string {
	s := num(v, "%.1f")
	if v != nil && missing > 0 {
		s += fmt.Sprintf("+%dh?", missing)
	}
	return s
}
