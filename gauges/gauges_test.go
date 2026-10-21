package gauges

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func fakeNLDI(t *testing.T) *httptest.Server {
	t.Helper()
	// Start reach 10 runs upstream->downstream (-105,40)->(-105,40.01). Upstream reach 11 and
	// downstream reach 9 each have one mainstem gauge; 12 is a tributary.
	mux := http.NewServeMux()
	mux.HandleFunc("/comid/position", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("coords"), "POINT(-105 40.005)") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"features":[{"geometry":{"type":"LineString","coordinates":[[-105,40],[-105,40.01]]},"properties":{"comid":10}}]}`)
	})
	site := func(id string, comid int, lat float64) string {
		return fmt.Sprintf(`{"geometry":{"type":"Point","coordinates":[-105,%v]},"properties":{"identifier":"USGS-%s","name":"G%s","comid":%d}}`, lat, id, id, comid)
	}
	// sameup carries an NLDI measure: 70% up from the downstream end, which is
	// upstream of the point (at ~50%).
	sameup := `{"geometry":{"type":"Point","coordinates":[-105,40.003]},"properties":{"identifier":"USGS-sameup","name":"Gsameup","comid":10,"measure":70}}`
	line := func(comid int, lat0, lat1 float64) string {
		return fmt.Sprintf(`{"geometry":{"type":"LineString","coordinates":[[-105,%v],[-105,%v]]},"properties":{"nhdplus_comid":%d}}`, lat0, lat1, comid)
	}
	fc := func(items ...string) string { return `{"features":[` + strings.Join(items, ",") + `]}` }
	mux.HandleFunc("/comid/10/navigation/UT/nwissite", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fc(site("same", 10, 40.007), sameup, site("up", 11, 40.02), site("trib", 12, 40.03)))
	})
	mux.HandleFunc("/comid/10/navigation/DD/nwissite", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fc(site("same", 10, 40.007), sameup, site("down", 9, 39.99)))
	})
	mux.HandleFunc("/comid/10/navigation/UM/flowlines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fc(line(10, 40, 40.01), line(11, 40.01, 40.03)))
	})
	mux.HandleFunc("/comid/10/navigation/DM/flowlines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fc(line(10, 40, 40.01), line(9, 39.98, 40)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFind(t *testing.T) {
	srv := fakeNLDI(t)
	c := NewClient(&Options{BaseURL: srv.URL})
	res, err := c.Find(context.Background(), 40.005, -105, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.COMID != 10 || res.SnapKm > 0.01 {
		t.Fatalf("snap: comid=%d snapKm=%v", res.COMID, res.SnapKm)
	}
	got := map[string]Gauge{}
	for _, g := range res.Gauges {
		got[g.SiteNo] = g
	}
	if len(got) != 5 {
		t.Fatalf("want 5 gauges (deduped), got %d: %+v", len(got), res.Gauges)
	}
	for id, want := range map[string]Direction{"same": Downstream, "sameup": Upstream, "up": Upstream, "down": Downstream, "trib": Upstream} {
		if got[id].Direction != want {
			t.Errorf("%s direction = %q, want %q", id, got[id].Direction, want)
		}
	}
	if n := got["same"].NetworkKm; n < 0.1 || n > 0.4 {
		t.Errorf("same network km = %v, want ~0.22", n)
	}
	if got["trib"].Mainstem || got["trib"].NetworkKm != -1 {
		t.Errorf("trib should have no network distance: %+v", got["trib"])
	}
	if !got["up"].Mainstem || got["up"].NetworkKm < 1 {
		t.Errorf("up network km = %v", got["up"].NetworkKm)
	}
	if res.Gauges[len(res.Gauges)-1].SiteNo != "trib" {
		t.Errorf("tributary gauge should sort last: %+v", res.Gauges)
	}
}

func TestFindNoCatchment(t *testing.T) {
	srv := fakeNLDI(t)
	c := NewClient(&Options{BaseURL: srv.URL})
	_, err := c.Find(context.Background(), 30, -30, 10)
	if !errors.Is(err, ErrNoCatchment) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindBadCoords(t *testing.T) {
	if _, err := NewClient(nil).Find(context.Background(), 100, 0, 1); err == nil {
		t.Fatal("want error")
	}
}

// Live check against the real NLDI; run with TRIBUTARY_LIVE=1.
func TestFindLive(t *testing.T) {
	if os.Getenv("TRIBUTARY_LIVE") == "" {
		t.Skip("set TRIBUTARY_LIVE=1 to hit the real NLDI")
	}
	res, err := NewClient(nil).Find(context.Background(), 40.0150, -105.2705, 25)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("comid=%d snap=%.3fkm gauges=%d", res.COMID, res.SnapKm, len(res.Gauges))
	for _, g := range res.Gauges {
		t.Logf("%-11s %-18s net=%6.1f straight=%5.1f %s", g.Direction, g.ID, g.NetworkKm, g.StraightKm, g.Name)
	}
	if len(res.Gauges) == 0 {
		t.Fatal("no gauges")
	}
}
