//go:build chdb

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A tiny dependency-free SVG chart emitter. GitHub renders committed .svg
// files inline in markdown, so the benchmark document can carry real
// comparison graphs with zero new Go dependencies (no charting lib, no
// CGO, go.mod stays clean). Everything here is hand-rolled string
// assembly of SVG primitives — rects for bars, polylines for the scaling
// curve, <text> for labels / axes / legend.
//
// The palette is colour-blind-safe (Okabe–Ito) and the charts are sized
// for a docs column width. Numbers are NOT baked into colour — every bar
// also carries a printed value label, so the chart reads in greyscale too.

const (
	chartW   = 720 // overall canvas width  (px)
	chartH   = 380 // overall canvas height (px)
	padL     = 70  // left padding (y axis + labels)
	padR     = 24
	padT     = 56 // top padding (title + legend)
	padB     = 64 // bottom padding (x labels + axis title)
	gridLine = "#d0d0d0"
	axisLine = "#333333"
	textCol  = "#222222"
	subText  = "#666666"
)

// okabeIto is the colour-blind-safe categorical palette (Okabe & Ito).
var okabeIto = []string{
	"#0072B2", // blue
	"#E69F00", // orange
	"#009E73", // green
	"#D55E00", // vermillion
	"#CC79A7", // purple
	"#56B4E9", // sky
}

// svgDoc accumulates SVG body fragments under a fixed viewBox.
type svgDoc struct {
	b strings.Builder
}

func newSVG() *svgDoc {
	d := &svgDoc{}
	fmt.Fprintf(&d.b,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" font-family="-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">`,
		chartW, chartH, chartW, chartH)
	fmt.Fprintf(&d.b, `<rect width="%d" height="%d" fill="white"/>`, chartW, chartH)
	return d
}

func (d *svgDoc) close() string { d.b.WriteString("</svg>\n"); return d.b.String() }

func (d *svgDoc) title(s string) {
	fmt.Fprintf(&d.b, `<text x="%d" y="26" font-size="18" font-weight="600" fill="%s">%s</text>`,
		padL, textCol, escapeXML(s))
}

func (d *svgDoc) text(x, y int, size int, fill, anchor, s string) {
	fmt.Fprintf(&d.b, `<text x="%d" y="%d" font-size="%d" fill="%s" text-anchor="%s">%s</text>`,
		x, y, size, fill, anchor, escapeXML(s))
}

func (d *svgDoc) line(x1, y1, x2, y2 int, col string, w float64) {
	fmt.Fprintf(&d.b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="%g"/>`,
		x1, y1, x2, y2, col, w)
}

func (d *svgDoc) rect(x, y, w, h int, fill string) {
	fmt.Fprintf(&d.b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x, y, w, h, fill)
}

// legend draws a horizontal legend swatch row centered under the title.
func (d *svgDoc) legend(labels []string) {
	x := padL
	const sw = 14
	for i, lab := range labels {
		col := okabeIto[i%len(okabeIto)]
		d.rect(x, padT-26, sw, sw, col)
		d.text(x+sw+5, padT-14, 12, textCol, "start", lab)
		x += sw + 9 + len(lab)*7 + 16
	}
}

// plotFrame draws the axes + horizontal gridlines for a numeric y range
// [0, ymax], returning the inner plot rect (x0,y0,x1,y1) and a y-mapper.
func (d *svgDoc) plotFrame(ymax float64, yUnit string, yticks int) (x0, y0, x1, y1 int, ymap func(float64) int) {
	x0, y0 = padL, padT
	x1, y1 = chartW-padR, chartH-padB
	ph := y1 - y0
	if ymax <= 0 {
		ymax = 1
	}
	ymap = func(v float64) int { return y1 - int(float64(ph)*(v/ymax)) }
	// gridlines + y tick labels
	if yticks < 1 {
		yticks = 4
	}
	for i := 0; i <= yticks; i++ {
		v := ymax * float64(i) / float64(yticks)
		yy := ymap(v)
		d.line(x0, yy, x1, yy, gridLine, 1)
		d.text(x0-8, yy+4, 11, subText, "end", fmtTick(v))
	}
	// axes
	d.line(x0, y0, x0, y1, axisLine, 1.5)
	d.line(x0, y1, x1, y1, axisLine, 1.5)
	// y axis unit (rotated)
	fmt.Fprintf(&d.b, `<text x="16" y="%d" font-size="12" fill="%s" text-anchor="middle" transform="rotate(-90 16 %d)">%s</text>`,
		(y0+y1)/2, subText, (y0+y1)/2, escapeXML(yUnit))
	return x0, y0, x1, y1, ymap
}

// groupedBars draws a grouped bar chart: one group per groupLabel, one bar
// per series within the group. values[g][s] is the value for group g,
// series s; series is the legend. ymax auto-scales with 10% headroom.
func groupedBars(title, yUnit string, groupLabels, series []string, values [][]float64, fmtVal func(float64) string) string {
	d := newSVG()
	d.title(title)
	d.legend(series)

	var ymax float64
	for _, row := range values {
		for _, v := range row {
			if v > ymax {
				ymax = v
			}
		}
	}
	ymax *= 1.15

	x0, _, x1, y1, ymap := d.plotFrame(ymax, yUnit, 5)
	plotW := x1 - x0
	nG := len(groupLabels)
	if nG == 0 {
		return d.close()
	}
	groupW := plotW / nG
	const groupGap = 18
	innerW := groupW - groupGap
	nS := len(series)
	barW := innerW / max1(nS)

	for g := 0; g < nG; g++ {
		gx := x0 + g*groupW + groupGap/2
		for sIdx := 0; sIdx < nS; sIdx++ {
			v := values[g][sIdx]
			bx := gx + sIdx*barW
			by := ymap(v)
			d.rect(bx, by, barW-3, y1-by, okabeIto[sIdx%len(okabeIto)])
			d.text(bx+(barW-3)/2, by-5, 10, textCol, "middle", fmtVal(v))
		}
		// group label
		d.text(gx+innerW/2, y1+18, 12, textCol, "middle", groupLabels[g])
	}
	return d.close()
}

// barChart draws a simple single-series bar chart (one bar per label).
func barChart(title, yUnit string, labels []string, values []float64, fmtVal func(float64) string) string {
	d := newSVG()
	d.title(title)

	var ymax float64
	for _, v := range values {
		if v > ymax {
			ymax = v
		}
	}
	ymax *= 1.15

	x0, _, x1, y1, ymap := d.plotFrame(ymax, yUnit, 5)
	plotW := x1 - x0
	n := len(labels)
	if n == 0 {
		return d.close()
	}
	slot := plotW / n
	const gap = 22
	barW := slot - gap
	for i := 0; i < n; i++ {
		v := values[i]
		bx := x0 + i*slot + gap/2
		by := ymap(v)
		d.rect(bx, by, barW, y1-by, okabeIto[i%len(okabeIto)])
		d.text(bx+barW/2, by-5, 11, textCol, "middle", fmtVal(v))
		d.text(bx+barW/2, y1+18, 11, textCol, "middle", labels[i])
	}
	return d.close()
}

// linePoint is one (x,y) on a line series.
type linePoint struct {
	X, Y  float64
	Label string // x-axis tick label
}

// lineChart draws a single line series with markers and value labels — used
// for wall-vs-cardinality scaling.
func lineChart(title, xUnit, yUnit string, pts []linePoint, fmtVal func(float64) string) string {
	d := newSVG()
	d.title(title)

	var ymax, xmax float64
	for _, p := range pts {
		if p.Y > ymax {
			ymax = p.Y
		}
		if p.X > xmax {
			xmax = p.X
		}
	}
	ymax *= 1.15
	if xmax <= 0 {
		xmax = 1
	}

	x0, _, x1, y1, ymap := d.plotFrame(ymax, yUnit, 5)
	plotW := x1 - x0
	xmap := func(v float64) int { return x0 + int(float64(plotW)*(v/xmax)) }

	// polyline
	var poly strings.Builder
	for _, p := range pts {
		fmt.Fprintf(&poly, "%d,%d ", xmap(p.X), ymap(p.Y))
	}
	fmt.Fprintf(&d.b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2.5"/>`,
		strings.TrimSpace(poly.String()), okabeIto[0])
	for _, p := range pts {
		px, py := xmap(p.X), ymap(p.Y)
		fmt.Fprintf(&d.b, `<circle cx="%d" cy="%d" r="4" fill="%s"/>`, px, py, okabeIto[0])
		d.text(px, py-9, 11, textCol, "middle", fmtVal(p.Y))
		d.text(px, y1+18, 11, textCol, "middle", p.Label)
	}
	// x axis unit
	d.text((x0+x1)/2, chartH-padB+44, 12, subText, "middle", xUnit)
	return d.close()
}

// writeChart writes one SVG file under docs/benchmarks/ relative to the doc
// output path, returning the markdown-relative reference path.
func writeChart(docOut, name, svg string) (string, error) {
	dir := filepath.Join(filepath.Dir(docOut), "benchmarks")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // doc dir
		return "", err
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte(svg), 0o644); err != nil { //nolint:gosec // doc artifact
		return "", err
	}
	return "benchmarks/" + name, nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// fmtTick renders a y-axis tick value compactly (k / M suffixes).
func fmtTick(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fk", v/1e3)
	case v == float64(int64(v)):
		return fmt.Sprintf("%d", int64(v))
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
