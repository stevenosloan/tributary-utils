package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseSites(t *testing.T) {
	in := `# header comment

02390000 headwater   # Amicalola Creek
  01646500	large river  
07056000
   # indented comment
`
	got, err := parseSites(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []site{{"02390000", "headwater"}, {"01646500", "large river"}, {"07056000", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseSitesErrors(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"not a number", "02390000 a\nfoo mid\n", "line 2"},
		{"too short", "\n\n1234\n", "line 3"},
		{"duplicate", "02390000 a\n# x\n02390000 b\n", "line 3"},
	}
	for _, tt := range tests {
		_, err := parseSites(strings.NewReader(tt.in))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want error mentioning %q", tt.name, err, tt.want)
		}
	}
}

func TestShippedSitesFile(t *testing.T) {
	ss, err := loadSites("sites.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 14 {
		t.Errorf("got %d sites, want 14", len(ss))
	}
	for _, s := range ss {
		if s.Label == "" {
			t.Errorf("site %s has no label", s.No)
		}
	}
}

func TestSiteCacheRoundTrip(t *testing.T) {
	path := t.TempDir() + "/sub/sites.json"
	c, err := loadSiteCache(path)
	if err != nil {
		t.Fatal(err)
	}
	c.set("1", siteInfo{COMID: 5, Gauge: "G"})
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	c2, err := loadSiteCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if i, ok := c2.get("1"); !ok || i.COMID != 5 || i.Gauge != "G" {
		t.Errorf("got %+v %v", i, ok)
	}
	os.WriteFile(path, []byte("{"), 0o644)
	if _, err := loadSiteCache(path); err == nil {
		t.Error("corrupt cache loaded without error")
	}
}
