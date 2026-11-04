package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stevenosloan/tributary-utils/internal/httpx"
)

// site is one line of a sites file.
type site struct {
	No    string // USGS site number, e.g. "02390000"
	Label string // free text used to group the report; may be empty
}

// parseSites reads a sites file: one gauge per line as "<siteNo> [label]".
// Everything from a '#' on is a comment, and blank lines are ignored. A bad
// line is an error naming its line number.
func parseSites(r io.Reader) ([]site, error) {
	var out []site
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !validSiteNo(f[0]) {
			return nil, fmt.Errorf("line %d: %q is not a USGS site number (8 to 15 digits)", n, f[0])
		}
		if prev, ok := seen[f[0]]; ok {
			return nil, fmt.Errorf("line %d: site %s already listed on line %d", n, f[0], prev)
		}
		seen[f[0]] = n
		out = append(out, site{No: f[0], Label: strings.Join(f[1:], " ")})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validSiteNo(s string) bool {
	if len(s) < 8 || len(s) > 15 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func loadSites(path string) ([]site, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ss, err := parseSites(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ss, nil
}

// siteInfo is what is known about a gauge's reach, kept in DIR/sites.json so
// later runs skip NLDI.
type siteInfo struct {
	COMID int    `json:"comid"`
	Gauge string `json:"gauge_name"`
	Reach string `json:"reach_name,omitempty"` // known after the first forecast
}

// siteCache is the contents of DIR/sites.json, safe for concurrent use.
type siteCache struct {
	path string
	mu   sync.Mutex
	m    map[string]siteInfo
	// dirty is set when m differs from what was loaded or last saved.
	dirty bool
}

func loadSiteCache(path string) (*siteCache, error) {
	c := &siteCache{path: path, m: map[string]siteInfo{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &c.m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.m == nil {
		c.m = map[string]siteInfo{}
	}
	return c, nil
}

func (c *siteCache) get(no string) (siteInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, ok := c.m[no]
	return i, ok
}

func (c *siteCache) set(no string, i siteInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.m[no]; ok && old == i {
		return
	}
	c.m[no] = i
	c.dirty = true
}

// save writes the cache if it changed since it was loaded or last saved.
func (c *siteCache) save() error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	b, err := json.MarshalIndent(c.m, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path, append(b, '\n')); err != nil {
		return err
	}
	c.mu.Lock()
	c.dirty = false
	c.mu.Unlock()
	return nil
}

// writeFileAtomic writes data to a temp file beside path, then renames it into
// place, creating directories as needed.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// nldiLookup asks NLDI for the reach COMID and name of a gauge. base is the
// NLDI linked-data URL.
func nldiLookup(ctx context.Context, hc *http.Client, base, siteNo string) (siteInfo, error) {
	u := strings.TrimRight(base, "/") + "/nwissite/USGS-" + siteNo
	status, body, err := httpx.Get(ctx, hc, u, nil)
	if err != nil {
		return siteInfo{}, fmt.Errorf("NLDI lookup: %w", err)
	}
	if status != http.StatusOK {
		return siteInfo{}, fmt.Errorf("NLDI lookup: %w", httpx.StatusError(status, body))
	}
	var fc struct {
		Features []struct {
			Properties struct {
				COMID json.Number `json:"comid"`
				Name  string      `json:"name"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &fc); err != nil {
		return siteInfo{}, fmt.Errorf("NLDI lookup: decoding response: %w", err)
	}
	if len(fc.Features) == 0 {
		return siteInfo{}, fmt.Errorf("NLDI lookup: no such gauge")
	}
	p := fc.Features[0].Properties
	id, err := p.COMID.Int64()
	if err != nil || id <= 0 {
		return siteInfo{}, fmt.Errorf("NLDI lookup: gauge has no reach COMID")
	}
	return siteInfo{COMID: int(id), Gauge: p.Name}, nil
}
