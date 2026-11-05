package main

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/stevenosloan/tributary-utils/readings"
)

const (
	// matchWindow is how far from a forecast's valid time a gauge value may be
	// and still count as the observation for it.
	matchWindow = 15 * time.Minute
	// maxLead is the last lead hour of a short-range forecast.
	maxLead = 18
)

// pair is one forecast point matched with a gauge value.
type pair struct {
	Site     string
	Label    string
	Lead     int // whole hours from the reference time to Time, 1..maxLead
	Time     time.Time
	Forecast float64
	Observed float64
}

// pairForecast matches each point of f with the gauge value nearest its valid
// time within matchWindow, from obs or, if it has none there, from fallback.
// Both must be sorted by time. Points with a lead outside 1..maxLead, in the
// future of now, or with no value nearby are skipped.
func pairForecast(f forecast, obs, fallback []readings.Value, now time.Time) []pair {
	var out []pair
	for _, p := range f.Points {
		lead := int(math.Round(p.Time.Sub(f.ReferenceTime).Hours()))
		if lead < 1 || lead > maxLead || p.Time.After(now) {
			continue
		}
		v, ok := nearest(obs, p.Time, matchWindow)
		if !ok {
			if v, ok = nearest(fallback, p.Time, matchWindow); !ok {
				continue
			}
		}
		out = append(out, pair{Site: f.Site, Label: f.Label, Lead: lead, Time: p.Time,
			Forecast: p.Flow, Observed: v.Value})
	}
	return out
}

// nearest returns the value of sorted obs closest to t, if within window.
func nearest(obs []readings.Value, t time.Time, window time.Duration) (readings.Value, bool) {
	i := sort.Search(len(obs), func(i int) bool { return !obs[i].Time.Before(t) })
	var best readings.Value
	bestD, found := window+1, false
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(obs) {
			continue
		}
		d := obs[j].Time.Sub(t).Abs()
		if d <= window && d < bestD {
			best, bestD, found = obs[j], d, true
		}
	}
	return best, found
}

// Stat summarizes the forecast errors of a set of pairs. The percent metrics
// leave out pairs whose observation is zero or negative (NPct counts the pairs
// used) and are 0 when NPct is 0; MAE uses every pair.
type Stat struct {
	N       int     `json:"n"`
	NPct    int     `json:"n_pct"`
	MedAPE  float64 `json:"median_abs_pct_error"` // percent
	MedBias float64 `json:"median_pct_bias"`      // percent; positive means the model is high
	MAE     float64 `json:"mae"`                  // in the flow unit
}

// computeStat computes the Stat of ps.
func computeStat(ps []pair) Stat {
	s := Stat{N: len(ps)}
	var ape, bias []float64
	var sum float64
	for _, p := range ps {
		sum += math.Abs(p.Forecast - p.Observed)
		if p.Observed > 0 {
			e := (p.Forecast - p.Observed) / p.Observed * 100
			bias = append(bias, e)
			ape = append(ape, math.Abs(e))
		}
	}
	s.NPct = len(ape)
	if s.N > 0 {
		s.MAE = sum / float64(s.N)
	}
	s.MedAPE, s.MedBias = median(ape), median(bias)
	return s
}

// median of xs; 0 for none.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// LeadStat is the Stat of one lead hour.
type LeadStat struct {
	Lead int `json:"lead"`
	Stat
}

// byLead computes the Stat of each lead hour that has pairs, in lead order.
func byLead(ps []pair) []LeadStat {
	groups := map[int][]pair{}
	for _, p := range ps {
		groups[p.Lead] = append(groups[p.Lead], p)
	}
	var out []LeadStat
	for l := 1; l <= maxLead; l++ {
		if g := groups[l]; len(g) > 0 {
			out = append(out, LeadStat{Lead: l, Stat: computeStat(g)})
		}
	}
	return out
}

// bucket is a range of lead hours, to keep the report readable.
type bucket struct {
	Name   string
	Lo, Hi int
}

var buckets = []bucket{{"1-3h", 1, 3}, {"4-6h", 4, 6}, {"7-12h", 7, 12}, {"13-18h", 13, 18}}

// BucketStat is the Stat of the pairs in one lead bucket.
type BucketStat struct {
	Bucket string `json:"bucket"`
	Stat
}

// byBucket pools the pairs of each lead bucket that has any.
func byBucket(ps []pair) []BucketStat {
	var out []BucketStat
	for _, b := range buckets {
		var g []pair
		for _, p := range ps {
			if p.Lead >= b.Lo && p.Lead <= b.Hi {
				g = append(g, p)
			}
		}
		if len(g) > 0 {
			out = append(out, BucketStat{Bucket: b.Name, Stat: computeStat(g)})
		}
	}
	return out
}

// nameStop lists words too common in gauge and reach names to show that they
// name the same stream.
var nameStop = map[string]bool{
	"the": true, "and": true, "river": true, "creek": true, "near": true, "above": true,
	"below": true, "fork": true, "branch": true, "stream": true, "run": true, "nr": true,
	"abv": true, "blw": true, "north": true, "south": true, "east": true, "west": true,
	"upper": true, "lower": true, "middle": true,
}

func significantWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if len(w) >= 3 && !nameStop[w] {
			out[w] = true
		}
	}
	return out
}

// namesMismatch reports whether a gauge's name and its reach's name share no
// significant word, which can mean NLDI indexed the gauge to the wrong reach.
// It is false when either name is unknown or has no significant word.
func namesMismatch(gauge, reach string) bool {
	g, r := significantWords(gauge), significantWords(reach)
	if len(g) == 0 || len(r) == 0 {
		return false
	}
	for w := range g {
		if r[w] {
			return false
		}
	}
	return true
}
