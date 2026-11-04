package main

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stevenosloan/tributary-utils/readings"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestMedian(t *testing.T) {
	for _, tt := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{5}, 5}, {[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}} {
		if got := median(tt.in); got != tt.want {
			t.Errorf("median(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
	in := []float64{3, 1, 2}
	median(in)
	if !reflect.DeepEqual(in, []float64{3, 1, 2}) {
		t.Error("median reordered its input")
	}
}

func TestComputeStat(t *testing.T) {
	ps := []pair{
		{Forecast: 110, Observed: 100}, // +10%
		{Forecast: 60, Observed: 100},  // -40%
		{Forecast: 130, Observed: 100}, // +30%
		{Forecast: 5, Observed: 0},     // percent skipped, MAE kept
	}
	s := computeStat(ps)
	if s.N != 4 || s.NPct != 3 {
		t.Fatalf("n = %d, npct = %d", s.N, s.NPct)
	}
	if !near(s.MedAPE, 30) || !near(s.MedBias, 10) {
		t.Errorf("median ape %v bias %v, want 30 and 10", s.MedAPE, s.MedBias)
	}
	if !near(s.MAE, (10+40+30+5)/4.0) {
		t.Errorf("mae = %v", s.MAE)
	}
	if low := computeStat([]pair{{Forecast: 50, Observed: 100}}); low.MedBias >= 0 {
		t.Errorf("model low should give negative bias, got %v", low.MedBias)
	}
	if z := computeStat([]pair{{Forecast: 1, Observed: 0}}); z.NPct != 0 || z.MedAPE != 0 || z.MAE != 1 {
		t.Errorf("all-zero obs: %+v", z)
	}
	if e := computeStat(nil); e != (Stat{}) {
		t.Errorf("empty: %+v", e)
	}
}

func TestByLeadAndBucket(t *testing.T) {
	ps := []pair{
		{Lead: 2, Forecast: 110, Observed: 100},
		{Lead: 2, Forecast: 90, Observed: 100},
		{Lead: 3, Forecast: 100, Observed: 100},
		{Lead: 13, Forecast: 150, Observed: 100},
	}
	bl := byLead(ps)
	if len(bl) != 3 || bl[0].Lead != 2 || bl[0].N != 2 || !near(bl[0].MedAPE, 10) || bl[2].Lead != 13 {
		t.Errorf("byLead = %+v", bl)
	}
	bb := byBucket(ps)
	if len(bb) != 2 || bb[0].Bucket != "1-3h" || bb[0].N != 3 || bb[1].Bucket != "13-18h" || !near(bb[1].MedAPE, 50) {
		t.Errorf("byBucket = %+v", bb)
	}
}

func obsAt(minutes int, v float64) readings.Value {
	return readings.Value{Time: t0.Add(time.Duration(minutes) * time.Minute), Value: v, SeriesID: "s"}
}

func TestPairForecast(t *testing.T) {
	f := forecast{Site: "s", Label: "mid", ReferenceTime: t0, Points: []forecastPoint{
		{t0.Add(1 * time.Hour), 10},                // obs 5 min away: matched to nearer (60 min)
		{t0.Add(2 * time.Hour), 20},                // nearest obs 16 min away: no match
		{t0.Add(3 * time.Hour), 30},                // obs exactly 15 min away: matches
		{t0.Add(4*time.Hour + 20*time.Second), 40}, // lead rounds to 4
		{t0.Add(5 * time.Hour), 50},                // future of now
		{t0, 99},                                   // lead 0
		{t0.Add(19 * time.Hour), 98},               // lead 19
	}}
	obs := []readings.Value{
		obsAt(55, 1), obsAt(60, 100), obsAt(75, 2), // around hour 1
		obsAt(120+16, 3), // hour 2: only 16 min off
		obsAt(180+15, 4), // hour 3
		obsAt(240, 5),    // hour 4
		obsAt(300, 6),
	}
	now := t0.Add(5*time.Hour - time.Minute)
	got := pairForecast(f, obs, nil, now)
	if len(got) != 3 {
		t.Fatalf("got %d pairs: %+v", len(got), got)
	}
	if got[0].Lead != 1 || got[0].Observed != 100 || got[0].Forecast != 10 || got[0].Label != "mid" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Lead != 3 || got[1].Observed != 4 {
		t.Errorf("second = %+v", got[1])
	}
	if got[2].Lead != 4 || got[2].Observed != 5 {
		t.Errorf("third = %+v (lead should round to 4)", got[2])
	}
}

func TestPairForecastFallback(t *testing.T) {
	f := forecast{Site: "s", ReferenceTime: t0, Points: []forecastPoint{
		{t0.Add(1 * time.Hour), 10}, {t0.Add(2 * time.Hour), 20}, {t0.Add(3 * time.Hour), 30}}}
	primary := []readings.Value{obsAt(60, 1)}
	other := []readings.Value{obsAt(65, 9), obsAt(120, 2)} // 65 is nearer than nothing but primary wins at 60
	got := pairForecast(f, primary, other, t0.Add(10*time.Hour))
	if len(got) != 2 || got[0].Observed != 1 || got[1].Observed != 2 {
		t.Errorf("got %+v, want primary at 1h and fallback at 2h, nothing at 3h", got)
	}
}

func TestNearestEmpty(t *testing.T) {
	if _, ok := nearest(nil, t0, matchWindow); ok {
		t.Error("matched in empty observations")
	}
}

func TestObservationsPicksLargestSeries(t *testing.T) {
	vs := []readings.Value{
		{SeriesID: "a", Value: 1}, {SeriesID: "b", Value: 2}, {SeriesID: "b", Value: 3},
		{SeriesID: "a", Value: 4}, {SeriesID: "b", Value: 5},
	}
	got, rest := splitSeries(vs)
	if len(got) != 3 || got[0].SeriesID != "b" || len(rest) != 2 {
		t.Errorf("got %+v, rest %+v", got, rest)
	}
}

func TestNamesMismatch(t *testing.T) {
	for _, tt := range []struct {
		gauge, reach string
		want         bool
	}{
		{"BEAVER KILL AT COOKS FALLS, NY", "Cook Brook", true},
		{"POTOMAC RIVER NEAR WASH, DC LITTLE FALLS PUMP STA", "Potomac River", false},
		{"Amicalola Creek near Dawsonville, GA", "Amicalola Creek", false},
		{"Clinch River above Tazewell", "Clinch River", false},
		{"Madison River below Ennis Lake", "Cherry Creek", true},
		{"Some Gauge", "", false},
		{"", "Some Reach", false},
		{"Fish River", "Creek", false}, // reach has no significant word
	} {
		if got := namesMismatch(tt.gauge, tt.reach); got != tt.want {
			t.Errorf("namesMismatch(%q, %q) = %v, want %v", tt.gauge, tt.reach, got, tt.want)
		}
	}
}

func TestLabelReports(t *testing.T) {
	sites := []SiteReport{{Site: "a", Label: "mid"}, {Site: "b", Label: "headwater"}, {Site: "c", Label: "mid"}, {Site: "d"}}
	ps := []pair{
		{Site: "a", Lead: 1, Forecast: 110, Observed: 100},
		{Site: "c", Lead: 1, Forecast: 130, Observed: 100},
		{Site: "b", Lead: 1, Forecast: 200, Observed: 100},
	}
	got := labelReports(sites, ps)
	if len(got) != 3 || got[0].Label != "(none)" || got[1].Label != "headwater" || got[2].Label != "mid" {
		t.Fatalf("labels = %+v", got)
	}
	if got[2].Sites != 2 || got[2].Overall.N != 2 || !near(got[2].Overall.MedAPE, 20) || !near(got[1].Overall.MedAPE, 100) {
		t.Errorf("mid = %+v, headwater = %+v", got[2], got[1])
	}
}
