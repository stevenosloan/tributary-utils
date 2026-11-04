// Command flowcheck measures how accurate National Water Model short-range
// forecasts are against USGS gauge discharge. Run record regularly (hourly) to
// save forecasts, and report any time to score what has been saved.
//
//	go run ./cmd/flowcheck record -sites cmd/flowcheck/sites.txt -dir ~/flowcheck
//	go run ./cmd/flowcheck report -dir ~/flowcheck
//	go run ./cmd/flowcheck report -dir ~/flowcheck -json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: flowcheck record -sites FILE -dir DIR [flags]")
	fmt.Fprintln(w, "       flowcheck report -dir DIR [flags]")
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "record":
		return runRecord(args[1:], stdout, stderr)
	case "report":
		return runReport(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "flowcheck: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func runRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sites := fs.String("sites", "", "sites file: one `<siteNo> [label]` per line (required)")
	dir := fs.String("dir", "", "data directory (required)")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	nldi := fs.String("nldi-url", "", "override NLDI base URL")
	nwps := fs.String("nwps-url", "", "override NWPS base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sites == "" || *dir == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "error: record needs -sites and -dir")
		return 2
	}
	ss, err := loadSites(*sites)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cfg := recordConfig{Sites: ss, Dir: *dir, NLDIURL: *nldi, NWPSURL: *nwps,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
	if err := record(ctx, cfg, stdout); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func runReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "data directory (required)")
	sites := fs.String("sites", "", "sites file whose labels replace the saved ones")
	asJSON := fs.Bool("json", false, "output JSON with full detail")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	usgs := fs.String("usgs-url", "", "override USGS Water Data API base URL")
	key := fs.String("api-key", os.Getenv("USGS_API_KEY"), "api.data.gov key (default $USGS_API_KEY)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "error: report needs -dir")
		return 2
	}
	cfg := reportConfig{Dir: *dir, USGSURL: *usgs, APIKey: *key, HTTP: &http.Client{Timeout: 30 * time.Second}}
	if *sites != "" {
		var err error
		if cfg.Labels, err = loadSites(*sites); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rep, err := report(ctx, cfg, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	printReport(stdout, rep)
	return 0
}
