package render

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/stevenosloan/tributary-utils/gauges"
)

func sample() *gauges.Result {
	return &gauges.Result{
		Lat: 40.005, Lon: -105, DistanceKm: 10, COMID: 10, SnapKm: 0.02,
		Reach:    gauges.Line{{-105, 40}, {-105, 40.01}},
		UpPath:   []gauges.Line{{{-105, 40.01}, {-105, 40.03}}},
		DownPath: []gauges.Line{{{-105, 39.98}, {-105, 40}}},
		Gauges: []gauges.Gauge{
			{SiteNo: "same", COMID: 10, Lat: 40.007, Lon: -105, Direction: gauges.Downstream, Mainstem: true, NetworkKm: 0.2, Via: []string{"UT", "DD"}},
			{SiteNo: "up<&>", COMID: 11, Lat: 40.02, Lon: -105, Direction: gauges.Upstream, Mainstem: true, NetworkKm: 1.7, Via: []string{"UT"}},
			{SiteNo: "trib", COMID: 12, Lat: 40.03, Lon: -104.99, Direction: gauges.Upstream, NetworkKm: -1, Via: []string{"UT"}},
		},
	}
}

func TestSVG(t *testing.T) {
	var buf bytes.Buffer
	if err := SVG(&buf, sample(), Options{Title: "t <1>"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("invalid XML: %v", err)
		}
	}
	for _, want := range []string{"t &lt;1&gt;", "up&lt;&amp;&gt;", "same reach", "on UM path", "tributary / diversion", "0.2 km"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Error("output contains NaN/Inf coordinates")
	}
}

func TestSVGManyGaugesTruncates(t *testing.T) {
	r := sample()
	for i := 0; i < 20; i++ {
		r.Gauges = append(r.Gauges, r.Gauges[0])
	}
	var buf bytes.Buffer
	if err := SVG(&buf, r, Options{MaxRows: 5}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "+18 more not shown") {
		t.Error("expected truncation note")
	}
}

func TestSVGNil(t *testing.T) {
	if err := SVG(io.Discard, nil, Options{}); err == nil {
		t.Fatal("want error")
	}
}
