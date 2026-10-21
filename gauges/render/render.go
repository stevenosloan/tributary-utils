// Package render draws a gauges.Result as a standalone SVG: a map of the river
// network around the point with numbered gauges, a zoomed inset of the start
// reach, and a table showing how the NLDI calls combined into each tag.
package render

import (
	"fmt"
	"html"
	"io"
	"math"
	"strings"

	"github.com/stevenosloan/tributary-utils/gauges"
)

// Options configures SVG.
type Options struct {
	Title   string // defaults to the coordinates
	MaxRows int    // table rows before "+N more"; defaults to 12
}

const (
	colStart  = "#6e40c9"
	colUp     = "#2f81f7"
	colDown   = "#e07b00"
	colTrib   = "#b6c2cf"
	colGauge  = "#1a7f37"
	colPoint  = "#cf222e"
	colText   = "#1f2328"
	colMuted  = "#57606a"
	colBorder = "#d0d7de"

	width   = 1240
	mapW    = 620
	panelX  = 680
	insetW  = 270
	insetH  = 200
	minH    = 620
	mapTop  = 40
	rowH    = 46
	legendH = 48
)

type legendRow struct{ color, name, desc string }

// SVG writes the drawing to w.
func SVG(w io.Writer, r *gauges.Result, o Options) error {
	if r == nil {
		return fmt.Errorf("render: nil result")
	}
	if o.MaxRows <= 0 {
		o.MaxRows = 12
	}
	if o.Title == "" {
		o.Title = fmt.Sprintf("%.5f, %.5f, %.0f km cap", r.Lat, r.Lon, r.DistanceKm)
	}

	legend := []legendRow{{colStart, "Snap", fmt.Sprintf("comid/position: start reach, point position, SnapKm %.2f", r.SnapKm)}}
	if len(r.Tributaries) > 0 {
		legend = append(legend, legendRow{colTrib, "UT / flowlines", "all upstream reaches incl. tributaries (context only)"})
	}
	legend = append(legend,
		legendRow{colUp, "UM / flowlines", "mainstem path upstream, distance to upstream gauges"},
		legendRow{colDown, "DM / flowlines", "mainstem path downstream, reach order and lengths"},
		legendRow{colGauge, "UT + DD / nwissite", "which gauges exist and which query returned each"})

	shown := r.Gauges
	if len(shown) > o.MaxRows {
		shown = shown[:o.MaxRows]
	}
	// Panel: title, legend, table heading+header, rows, footnote.
	panelEnd := 70 + 20 + len(legend)*legendH + 70 + len(shown)*rowH + 60
	if len(r.Gauges) > len(shown) {
		panelEnd += 20
	}
	totalH := max(minH, panelEnd+mapTop/2)
	mapH := totalH - mapTop - 20

	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	p(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="13">`, width, totalH)
	p(`<rect width="%d" height="%d" rx="10" fill="#ffffff" stroke="%s"/>`, width, totalH, colBorder)
	p(`<text x="24" y="30" font-size="16" font-weight="600" fill="%s">%s</text>`, colText, html.EscapeString(o.Title))

	// Map.
	mx, my := 10.0, float64(mapTop)
	p(`<rect x="%g" y="%g" width="%d" height="%d" rx="6" fill="#f6f8fa"/>`, mx, my, mapW, mapH)
	all := collect(r)
	main := fit(bounds(all), mx, my, mapW, float64(mapH), 30)
	drawNetwork(p, main, r, 1.5, 3.5, 7)
	point := gauges.Line{{r.Lon, r.Lat}}
	for i, g := range r.Gauges {
		x, y := main.xy(g.Lon, g.Lat)
		p(`<circle cx="%.1f" cy="%.1f" r="7" fill="%s" stroke="#fff" stroke-width="2"/>`, x, y, colGauge)
		p(`<text x="%.0f" y="%.0f" font-weight="700" fill="%s" font-size="15">%d</text>`, x+10, y-10, colGauge, i+1)
	}
	px, py := main.xy(point[0][0], point[0][1])
	p(`<circle cx="%.1f" cy="%.1f" r="5" fill="%s" stroke="#fff" stroke-width="2"/>`, px, py, colPoint)

	// Inset around the point, in the emptiest corner of the map.
	ilat := 0.011
	ilon := ilat / math.Cos(r.Lat*math.Pi/180) * insetW / insetH
	ib := box{r.Lon - ilon, r.Lat - ilat, r.Lon + ilon, r.Lat + ilat}
	ix, iy := emptiestCorner(main, all, mx, my, float64(mapH))
	inset := fit(ib, ix, iy, insetW, insetH, 0)
	p(`<defs><clipPath id="ic"><rect x="%g" y="%g" width="%d" height="%d" rx="4"/></clipPath></defs>`, ix, iy, insetW, insetH)
	p(`<rect x="%g" y="%g" width="%d" height="%d" rx="4" fill="#fff" stroke="#8c959f"/>`, ix, iy, insetW, insetH)
	p(`<g clip-path="url(#ic)">`)
	drawNetwork(p, inset, r, 1.5, 3, 6)
	for i, g := range r.Gauges {
		x, y := inset.xy(g.Lon, g.Lat)
		p(`<circle cx="%.1f" cy="%.1f" r="7" fill="%s" stroke="#fff" stroke-width="2"/>`, x, y, colGauge)
		p(`<text x="%.0f" y="%.0f" font-weight="700" fill="%s" font-size="15">%d</text>`, x+10, y-10, colGauge, i+1)
	}
	cx, cy := inset.xy(r.Lon, r.Lat)
	p(`<circle cx="%.1f" cy="%.1f" r="6" fill="%s" stroke="#fff" stroke-width="2"/>`, cx, cy, colPoint)
	p(`</g>`)
	kmAcross := 2 * ilon * math.Cos(r.Lat*math.Pi/180) * 111.195
	p(`<text x="%g" y="%g" font-size="12" font-weight="600" fill="%s">Zoom, about %.1f km across</text>`, ix+8, iy+16, colText, kmAcross)
	p(`<text x="%g" y="%g" font-size="11" fill="%s">purple = start reach (comid %d)</text>`, ix+8, iy+insetH-8, colStart, r.COMID)
	zx0, zy0 := main.xy(ib.x0, ib.y1)
	zx1, zy1 := main.xy(ib.x1, ib.y0)
	p(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="none" stroke="#8c959f" stroke-dasharray="3 3"/>`, zx0, zy0, zx1-zx0, zy1-zy0)

	// Panel: which call contributes what.
	y := 82.0
	p(`<text x="%d" y="%g" font-size="15" font-weight="600" fill="%s">Which call contributes what</text>`, panelX, y, colText)
	y += 22
	for _, l := range legend {
		p(`<rect x="%d" y="%g" width="26" height="8" rx="4" fill="%s"/>`, panelX, y, l.color)
		p(`<text x="%d" y="%g" font-weight="600" fill="%s">%s</text>`, panelX+38, y+9, colText, html.EscapeString(l.name))
		p(`<text x="%d" y="%g" fill="%s" font-size="12">%s</text>`, panelX+38, y+27, colMuted, html.EscapeString(l.desc))
		y += legendH
	}

	// Panel: combined result.
	y += 22
	p(`<text x="%d" y="%g" font-size="15" font-weight="600" fill="%s">Combined result</text>`, panelX, y, colText)
	y += 24
	cols := []int{panelX, panelX + 26, panelX + 210, panelX + 370, panelX + 465}
	for i, t := range []string{"#", "Gauge", "Reach vs point", "Tag", "NetworkKm"} {
		p(`<text x="%d" y="%g" font-weight="600" fill="%s" font-size="12">%s</text>`, cols[i], y, colMuted, t)
	}
	p(`<line x1="%d" y1="%g" x2="%d" y2="%g" stroke="%s"/>`, panelX, y+8, panelX+540, y+8, colBorder)
	y += 32
	for i, g := range shown {
		how1, how2 := reachNote(r, g)
		nk := "-"
		if g.NetworkKm >= 0 {
			nk = fmt.Sprintf("%.1f km", g.NetworkKm)
		}
		p(`<text x="%d" y="%g" fill="%s" font-weight="700">%d</text>`, cols[0], y, colGauge, i+1)
		p(`<text x="%d" y="%g" fill="%s">%s</text>`, cols[1], y, colText, html.EscapeString(g.SiteNo))
		p(`<text x="%d" y="%g" fill="%s" font-size="11">%s</text>`, cols[1], y+16, colMuted, html.EscapeString(strings.Join(g.Via, ", ")))
		p(`<text x="%d" y="%g" fill="%s" font-size="12">%s</text>`, cols[2], y, colText, how1)
		p(`<text x="%d" y="%g" fill="%s" font-size="11">%s</text>`, cols[2], y+16, colMuted, how2)
		p(`<text x="%d" y="%g" fill="%s">%s</text>`, cols[3], y, colText, g.Direction)
		p(`<text x="%d" y="%g" fill="%s">%s</text>`, cols[4], y, colText, nk)
		y += rowH
	}
	if extra := len(r.Gauges) - len(shown); extra > 0 {
		p(`<text x="%d" y="%g" fill="%s" font-size="12">+%d more not shown</text>`, panelX, y-10, colMuted, extra)
		y += 20
	}
	p(`<text x="%d" y="%g" fill="%s" font-size="12">A gauge on the start reach is returned by both UT and DD, so its side is</text>`, panelX, y+4, colMuted)
	p(`<text x="%d" y="%g" fill="%s" font-size="12">decided by its measure against the point's position along the reach.</text>`, panelX, y+22, colMuted)
	p(`</svg>`)

	_, err := io.WriteString(w, b.String())
	return err
}

// reachNote describes how a gauge's direction and distance were derived.
func reachNote(r *gauges.Result, g gauges.Gauge) (string, string) {
	switch {
	case g.COMID == r.COMID:
		return "same reach", "compare measure"
	case g.Mainstem && g.Direction == gauges.Upstream:
		return "different reach", "on UM path"
	case g.Mainstem:
		return "different reach", "on DM path"
	default:
		return "off mainstem", "tributary / diversion"
	}
}

func drawNetwork(p func(string, ...any), pr proj, r *gauges.Result, thin, thick, start float64) {
	for _, l := range r.Tributaries {
		p(`<path d="%s" fill="none" stroke="%s" stroke-width="%g"/>`, pr.path(l), colTrib, thin)
	}
	for _, l := range r.UpPath {
		p(`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round"/>`, pr.path(l), colUp, thick)
	}
	for _, l := range r.DownPath {
		p(`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round"/>`, pr.path(l), colDown, thick)
	}
	p(`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round" opacity=".85"/>`, pr.path(r.Reach), colStart, start)
}

// --- projection ---

type box struct{ x0, y0, x1, y1 float64 } // lon/lat extent

func collect(r *gauges.Result) []gauges.Line {
	all := []gauges.Line{r.Reach, {{r.Lon, r.Lat}}}
	all = append(all, r.UpPath...)
	all = append(all, r.DownPath...)
	all = append(all, r.Tributaries...)
	for _, g := range r.Gauges {
		all = append(all, gauges.Line{{g.Lon, g.Lat}})
	}
	return all
}

func bounds(ls []gauges.Line) box {
	b := box{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, l := range ls {
		for _, p := range l {
			b.x0, b.x1 = math.Min(b.x0, p[0]), math.Max(b.x1, p[0])
			b.y0, b.y1 = math.Min(b.y0, p[1]), math.Max(b.y1, p[1])
		}
	}
	return b
}

// proj maps lon/lat to SVG coordinates with an equal-aspect (cos lat) fit.
type proj struct {
	b          box
	cs, sc     float64
	ox, oy, bh float64 // x origin; offset up from the box bottom; box bottom y
}

func fit(b box, x, y, w, h, margin float64) proj {
	cs := math.Cos((b.y0 + b.y1) / 2 * math.Pi / 180)
	dx, dy := (b.x1-b.x0)*cs, b.y1-b.y0
	sc := math.Min((w-2*margin)/math.Max(dx, 1e-9), (h-2*margin)/math.Max(dy, 1e-9))
	return proj{
		b: b, cs: cs, sc: sc,
		ox: x + margin + ((w-2*margin)-dx*sc)/2,
		oy: margin + ((h-2*margin)-dy*sc)/2,
		bh: y + h,
	}
}

func (pr proj) xy(lon, lat float64) (float64, float64) {
	return pr.ox + (lon-pr.b.x0)*pr.cs*pr.sc, pr.bh - pr.oy - (lat-pr.b.y0)*pr.sc
}

func (pr proj) path(l gauges.Line) string {
	var sb strings.Builder
	for i, p := range l {
		x, y := pr.xy(p[0], p[1])
		if i == 0 {
			fmt.Fprintf(&sb, "M%.1f,%.1f", x, y)
		} else {
			fmt.Fprintf(&sb, " L%.1f,%.1f", x, y)
		}
	}
	return sb.String()
}

// emptiestCorner returns the top-left of the inset placed in whichever map
// corner has the fewest vertices under it.
func emptiestCorner(pr proj, all []gauges.Line, mx, my, mh float64) (float64, float64) {
	const pad = 14
	corners := [][2]float64{
		{mx + pad, my + mh - insetH - pad}, // bottom-left
		{mx + mapW - insetW - pad, my + mh - insetH - pad},
		{mx + pad, my + pad},
		{mx + mapW - insetW - pad, my + pad},
	}
	best, bestN := corners[0], math.MaxInt
	for _, c := range corners {
		n := 0
		for _, l := range all {
			for _, p := range l {
				x, y := pr.xy(p[0], p[1])
				if x >= c[0] && x <= c[0]+insetW && y >= c[1] && y <= c[1]+insetH {
					n++
				}
			}
		}
		if n < bestN {
			best, bestN = c, n
		}
	}
	return best[0], best[1]
}
