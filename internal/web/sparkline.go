package web

import (
	"strconv"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/store"
)

// Sparkline geometry, in pixels: the plot is sparkHeight tall with
// sparkPad above and below, so 2px lines and 8px dots are never clipped.
const (
	sparkHeight = 48
	sparkPad    = 6
)

// sparkline is a latency chart of a check's recent probes, drawn as SVG.
//
// x positions are percentages of the width, so the chart stretches to any
// width without distorting its strokes and dots, as scaling a viewBox
// would. The width is divided into recentProbes equal slots and each probe
// sits in the middle of its own, counted from the right edge, so a check
// with few probes so far fills in from the right instead of spreading them
// across the whole chart.
type sparkline struct {
	Segments []segment // the latency line, broken at failed probes
	Failures []band    // runs of failed probes, shaded
	Hits     []hit     // invisible hover areas with a tooltip per probe
	End      *dot      // the latest probe, if it succeeded
	Count    int       // probes shown
	// Caption summarises the chart: how many probes, the peak latency and
	// how many failed. It doubles as the chart's accessible name.
	Caption string
}

type segment struct{ X1, Y1, X2, Y2 string }
type band struct{ X, Width string }
type dot struct{ X, Y string }
type hit struct{ X, Width, Title string }

func buildSparkline(samples []store.Sample) sparkline {
	if len(samples) > recentProbes {
		samples = samples[len(samples)-recentProbes:]
	}
	var peak time.Duration
	for _, s := range samples {
		if s.OK {
			peak = max(peak, s.Duration)
		}
	}
	sp := sparkline{Count: len(samples)}
	slotWidth := 100.0 / recentProbes
	first := recentProbes - len(samples) // slot of the oldest sample

	left := func(i int) float64 { return float64(first+i) * slotWidth }
	right := func(i int) float64 { return left(i) + slotWidth }
	x := func(i int) float64 { return left(i) + slotWidth/2 }
	y := func(d time.Duration) float64 {
		if peak == 0 {
			return sparkHeight - sparkPad
		}
		return sparkHeight - sparkPad - float64(d)/float64(peak)*(sparkHeight-2*sparkPad)
	}
	runStart, failed := -1, 0 // first probe of the current run of failures
	for i, s := range samples {
		sp.Hits = append(sp.Hits, hit{X: pct(left(i)), Width: pct(right(i) - left(i)), Title: hoverText(s)})
		if !s.OK {
			failed++
			if runStart < 0 {
				runStart = i
			}
			if i == len(samples)-1 || samples[i+1].OK {
				sp.Failures = append(sp.Failures, band{X: pct(left(runStart)), Width: pct(right(i) - left(runStart))})
				runStart = -1
			}
			continue
		}
		if i > 0 && samples[i-1].OK {
			sp.Segments = append(sp.Segments, segment{
				X1: pct(x(i - 1)), Y1: px(y(samples[i-1].Duration)),
				X2: pct(x(i)), Y2: px(y(s.Duration)),
			})
		}
	}
	if last := len(samples) - 1; last >= 0 && samples[last].OK {
		sp.End = &dot{X: pct(x(last)), Y: px(y(samples[last].Duration))}
	}
	sp.Caption = caption(len(samples), failed, peak)
	return sp
}

// caption reads like "Last 60 probes, peak 230 ms, 2 failed". The peak
// covers successful probes only, so it is left out when there are none.
func caption(count, failed int, peak time.Duration) string {
	c := "Last probe"
	if count > 1 {
		c = "Last " + strconv.Itoa(count) + " probes"
	}
	if failed < count {
		c += ", peak " + latency(peak)
	}
	switch {
	case failed == 0:
	case failed == count && count == 1:
		c += ", failed"
	case failed == count:
		c += ", all failed"
	default:
		c += ", " + strconv.Itoa(failed) + " failed"
	}
	return c
}

func hoverText(s store.Sample) string {
	at := s.Start.UTC().Format("Jan 2 15:04:05 MST")
	if s.OK {
		return at + ": " + latency(s.Duration)
	}
	return at + ": failed, " + reasonText(s.Reason)
}

func pct(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + "%" }
func px(f float64) string  { return strconv.FormatFloat(f, 'f', 1, 64) }
