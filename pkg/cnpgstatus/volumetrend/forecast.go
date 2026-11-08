// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package volumetrend

import (
	"math"
	"sort"
	"time"
)

// The forecast, stated so a reader can check it by hand:
//
//   - Window: the last FitWindow (7 days) of samples, or what exists of
//     it. Seven days hold a weekly cycle (a Sunday batch, a nightly
//     VACUUM) that a one-day window would read as a trend.
//   - Reduction: the 15-minute samples are reduced to hourly medians,
//     so a burst inside one hour weighs one point, not four.
//   - Fit: Theil–Sen, the median of the slopes between every pair of
//     hourly points. A one-off drop (a VACUUM FULL, WAL recycled after
//     archiving catches up) or a spike moves a least-squares line; it
//     moves the median of pairwise slopes very little.
//   - Refusals: fewer than MinHours hourly points, or a window shorter
//     than MinSpan, is "too few points"; a median slope that would not
//     consume a visible share of the volume over the window is "not
//     growing". Neither is a forecast of "never".
//   - Faster than linear: when the second half of the window grows at
//     least AccelerationRatio times as fast as the first, the estimate
//     uses the second half's rate and says so: a linear extrapolation
//     of an accelerating curve is late, so the earlier date is the
//     honest one.
//   - Resizes: growing a filesystem shifts consumed (size - available)
//     by the reserved blocks the growth adds (ext4 reserves 5 % by
//     default), a level shift a fit would read as growth. When the size
//     changed inside the window, the fit uses the readings since the
//     last change if they are enough on their own; otherwise the
//     readings before each change are shifted by the jump measured
//     across it. An inode total that changes inside the window (XFS
//     allocates inodes on demand) leaves no inode forecast.
//   - Range and confidence: the slopes' interquartile range gives an
//     earliest and a latest date; a range wide against the median is
//     low confidence.
//   - Targets: the bytes forecast always runs toward the current
//     filesystem, autoGrow or not: an expansion that is refused, slow
//     or outpaced leaves PostgreSQL writing into this filesystem, and
//     one with almost nothing left is full now (FullFloor). With
//     autoGrow and a readable maxSize above the current size, a second
//     forecast runs toward maxSize, where the operator stops growing it.
const (
	FitWindow         = 7 * 24 * time.Hour
	MinSpan           = 24 * time.Hour
	MinHours          = 24
	AccelerationRatio = 1.5
	// halfMinHours is the least each half of the window needs for the
	// acceleration check.
	halfMinHours = 12
)

// FullFloor is the free space below which a filesystem is full for
// PostgreSQL: one 16 MiB WAL segment, or 0.05 % of the filesystem when
// that is more. Writing stops being reliable well before zero.
func FullFloor(totalBytes uint64) float64 {
	return math.Max(16<<20, 0.0005*float64(totalBytes))
}

// Measures.
const (
	MeasureBytes  = "bytes"
	MeasureInodes = "inodes"
)

// State is what a forecast could say.
type State string

// The forecast states.
const (
	// StateForecast: a time until full, with its rate and range.
	StateForecast State = "forecast"
	// StateNotGrowing: the fit shows no growth worth extrapolating.
	StateNotGrowing State = "notGrowing"
	// StateTooFewPoints: not enough samples to fit.
	StateTooFewPoints State = "tooFewPoints"
	// StateFull: nothing left to consume.
	StateFull State = "full"
	// StateNoData: the measure is not reported (a filesystem without an
	// inode count).
	StateNoData State = "noData"
	// StateTotalChanged: the inode total changed inside the window (XFS
	// allocates inodes on demand): there is no fixed line to run toward.
	StateTotalChanged State = "totalChanged"
)

// Target is the line a forecast runs toward.
type Target string

// The targets.
const (
	TargetFilesystem Target = "filesystem"
	TargetAutoGrow   Target = "autoGrowMaxSize"
)

// Confidence grades a forecast by the spread of its slopes.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Options carry what the volume's Cluster declares.
type Options struct {
	// AutoGrow is true when the volume's storage section has autoGrow.
	AutoGrow bool
	// AutoGrowMaxBytes is autoGrow.maxSize in bytes, nil when it could
	// not be read.
	AutoGrowMaxBytes *uint64
}

// Forecast is the time until one measure of one volume is exhausted.
type Forecast struct {
	Measure string
	State   State
	// Consumed is what the filesystem no longer gives PostgreSQL now
	// (bytes or inodes), Capacity the line it runs toward, Headroom the
	// difference.
	Consumed, Capacity, Headroom float64
	Target                       Target
	// AutoGrowSteps: the operator may grow the PVC, in steps, before the
	// estimate (autoGrow is set and its maxSize is not reached).
	AutoGrowSteps bool
	// AsOf is the reading the estimate starts from.
	AsOf time.Time
	// RatePerDay is the rate extrapolated; WindowStart and WindowEnd the
	// window fitted and WindowHours its hourly points.
	RatePerDay               float64
	WindowStart, WindowEnd   time.Time
	WindowHours              int
	FullAt                   *time.Time
	EarliestAt, LatestAt     *time.Time
	Confidence               string
	Accelerating             bool
	FirstHalfRatePerDay      float64
	RecentRatePerDay         float64
	MinimumGrowthOverWindow  float64
	MinimumHours, HoursFound int
	// ResizedAt is the last size change inside the fitted window; with
	// it, FitSinceResize says the fit starts at the change, else the
	// earlier readings were shifted by the jump across it.
	ResizedAt      *time.Time
	FitSinceResize bool
}

// fit is the Theil–Sen fit of one measure, per hour.
type fit struct {
	hours            int
	start, end       time.Time
	rate, low, high  float64
	first, recent    float64
	halvesComparable bool
}

type cachedFit struct {
	version       uint64
	bytes, inodes fit
	window        windowFacts
}

// windowFacts are what the window's size changes did to the fit.
type windowFacts struct {
	resizedAt      *time.Time
	fitSinceResize bool
	inodesChanged  bool
}

// Forecasts returns the forecasts of one volume: bytes toward the
// current filesystem, bytes toward autoGrow.maxSize (nil unless autoGrow
// is set with a readable maxSize above the current size) and inodes; ok
// is false for a volume the store does not hold.
func (s *Store) Forecasts(key Key, opts Options) (bytes Forecast, autoGrow *Forecast, inodes Forecast, ok bool) {
	s.mu.Lock()
	ser := s.series[key]
	if ser == nil {
		s.mu.Unlock()
		return Forecast{}, nil, Forecast{}, false
	}
	latest := ser.latest
	cached, hit := s.fits[key]
	if !hit || cached.version != ser.version {
		b, i, w := s.hourlyLocked(ser)
		cached = cachedFit{version: ser.version, bytes: theilSen(b), inodes: theilSen(i), window: w}
		s.fits[key] = cached
	}
	s.mu.Unlock()

	bytes = forecastBytes(latest, cached.bytes, opts)
	autoGrow = forecastAutoGrow(latest, cached.bytes, opts)
	bytes.ResizedAt, bytes.FitSinceResize = cached.window.resizedAt, cached.window.fitSinceResize
	if autoGrow != nil {
		autoGrow.ResizedAt, autoGrow.FitSinceResize = cached.window.resizedAt, cached.window.fitSinceResize
	}
	inodes = forecastInodes(latest, cached.inodes, opts)
	if cached.window.inodesChanged && inodes.State != StateNoData {
		inodes = Forecast{
			Measure: MeasureInodes, State: StateTotalChanged, Target: TargetFilesystem, AsOf: latest.At,
			Consumed: inodes.Consumed, Capacity: inodes.Capacity, Headroom: inodes.Headroom, AutoGrowSteps: inodes.AutoGrowSteps,
			MinimumHours: MinHours, HoursFound: cached.inodes.hours,
		}
		if inodes.Headroom <= 0 {
			inodes.State = StateFull
		}
	}
	return bytes, autoGrow, inodes, true
}

// hourlyPoint is one hourly median.
type hourlyPoint struct {
	at    time.Time
	value float64
}

// hourlyLocked reduces the last FitWindow of a series to hourly medians,
// handling the size changes inside it (see the method above).
func (s *Store) hourlyLocked(ser *series) (bytes, inodes []hourlyPoint, w windowFacts) {
	if len(ser.buf) == 0 {
		return nil, nil, w
	}
	// The window is the FitWindow/hour hourly buckets ending with the
	// newest sample's hour, the current partial hour included.
	from := s.slotTime(ser.newest).Truncate(time.Hour).Add(-FitWindow)
	type sample struct {
		slot   int64
		bytes  float64
		inodes float64
	}
	var samples []sample
	ser.each(func(slot int64, p point) {
		if !s.slotTime(slot).Truncate(time.Hour).After(from) {
			return
		}
		samples = append(samples, sample{slot: slot, bytes: float64(p.bytes), inodes: float64(p.inodes)})
	})
	if len(samples) == 0 {
		return nil, nil, w
	}
	// The size changes inside the window; the first step is the series'
	// own start, not a change.
	var resizes []int64
	for i := 1; i < len(ser.steps); i++ {
		st, prev := ser.steps[i], ser.steps[i-1]
		if st.slot <= samples[0].slot {
			continue
		}
		if st.total != prev.total {
			resizes = append(resizes, st.slot)
		}
		if !sameFloat(st.inodes, prev.inodes) {
			w.inodesChanged = true
		}
	}
	if n := len(resizes); n > 0 {
		last := resizes[n-1]
		at := s.slotTime(last)
		w.resizedAt = &at
		cut := sort.Search(len(samples), func(i int) bool { return samples[i].slot >= last })
		// Enough on their own: MinHours distinct hours with a byte
		// reading, spanning MinSpan.
		hours := map[int64]bool{}
		var first, lastAt time.Time
		for _, sm := range samples[cut:] {
			if math.IsNaN(sm.bytes) {
				continue
			}
			at := s.slotTime(sm.slot).Truncate(time.Hour)
			if first.IsZero() {
				first = at
			}
			lastAt = at
			hours[at.Unix()] = true
		}
		if len(hours) >= MinHours && lastAt.Sub(first) >= MinSpan {
			samples, w.fitSinceResize = samples[cut:], true
		} else {
			// Measure every jump on the raw values first, then shift
			// the readings before each change up to the level after it.
			type jump struct {
				before int
				delta  float64
			}
			var jumps []jump
			for _, r := range resizes {
				i := sort.Search(len(samples), func(i int) bool { return samples[i].slot >= r && !math.IsNaN(samples[i].bytes) })
				j := i - 1
				for j >= 0 && math.IsNaN(samples[j].bytes) {
					j--
				}
				if i < len(samples) && j >= 0 {
					jumps = append(jumps, jump{before: i, delta: samples[i].bytes - samples[j].bytes})
				}
			}
			for _, jp := range jumps {
				for k := range jp.before {
					samples[k].bytes += jp.delta
				}
			}
		}
	}
	type acc struct{ b, i []float64 }
	buckets := map[int64]*acc{}
	var order []int64
	for _, sm := range samples {
		hour := s.slotTime(sm.slot).Truncate(time.Hour).Unix()
		a := buckets[hour]
		if a == nil {
			a = &acc{}
			buckets[hour] = a
			order = append(order, hour)
		}
		if !math.IsNaN(sm.bytes) {
			a.b = append(a.b, sm.bytes)
		}
		if !math.IsNaN(sm.inodes) {
			a.i = append(a.i, sm.inodes)
		}
	}
	for _, hour := range order {
		a := buckets[hour]
		at := time.Unix(hour, 0).UTC()
		if len(a.b) > 0 {
			bytes = append(bytes, hourlyPoint{at: at, value: median(a.b)})
		}
		if len(a.i) > 0 {
			inodes = append(inodes, hourlyPoint{at: at, value: median(a.i)})
		}
	}
	return bytes, inodes, w
}

// theilSen fits the hourly points. A fit of fewer than two points has
// hours set and nothing else.
func theilSen(points []hourlyPoint) fit {
	f := fit{hours: len(points)}
	if len(points) < 2 {
		return f
	}
	f.start, f.end = points[0].at, points[len(points)-1].at
	slopes := pairwiseSlopes(points)
	f.rate, f.low, f.high = quantile(slopes, 0.5), quantile(slopes, 0.25), quantile(slopes, 0.75)
	half := len(points) / 2
	if half >= halfMinHours && len(points)-half >= halfMinHours {
		f.first = quantile(pairwiseSlopes(points[:half]), 0.5)
		f.recent = quantile(pairwiseSlopes(points[half:]), 0.5)
		f.halvesComparable = true
	}
	return f
}

// pairwiseSlopes are the per-hour slopes of every pair, sorted.
func pairwiseSlopes(points []hourlyPoint) []float64 {
	slopes := make([]float64, 0, len(points)*(len(points)-1)/2)
	for i := range points {
		for j := i + 1; j < len(points); j++ {
			dt := points[j].at.Sub(points[i].at).Hours()
			if dt <= 0 {
				continue
			}
			slopes = append(slopes, (points[j].value-points[i].value)/dt)
		}
	}
	sort.Float64s(slopes)
	return slopes
}

// quantile of sorted values, by linear interpolation; 0 when empty.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// autoGrowAhead reports whether autoGrow may still step the PVC.
func autoGrowAhead(latest Reading, opts Options) bool {
	return opts.AutoGrow && (opts.AutoGrowMaxBytes == nil || *opts.AutoGrowMaxBytes > latest.TotalBytes)
}

// forecastBytes runs toward the current filesystem, whatever autoGrow
// may do: a filesystem at its floor is full now.
func forecastBytes(latest Reading, f fit, opts Options) Forecast {
	out := Forecast{
		Measure: MeasureBytes, Consumed: latest.consumed(), Target: TargetFilesystem, AsOf: latest.At,
		Capacity: float64(latest.TotalBytes), Headroom: float64(latest.AvailableBytes),
		AutoGrowSteps: autoGrowAhead(latest, opts),
	}
	minimum := math.Max(1<<20, 0.0005*out.Capacity)
	return extrapolate(out, f, minimum, FullFloor(latest.TotalBytes))
}

// forecastAutoGrow runs toward autoGrow.maxSize: the headroom is what is
// free now plus what the steps may add.
func forecastAutoGrow(latest Reading, f fit, opts Options) *Forecast {
	maxBytes := opts.AutoGrowMaxBytes
	if !opts.AutoGrow || maxBytes == nil || *maxBytes <= latest.TotalBytes {
		return nil
	}
	out := Forecast{
		Measure: MeasureBytes, Consumed: latest.consumed(), Target: TargetAutoGrow, AsOf: latest.At, AutoGrowSteps: true,
		Headroom: float64(latest.AvailableBytes) + float64(*maxBytes-latest.TotalBytes),
	}
	out.Capacity = out.Consumed + out.Headroom
	minimum := math.Max(1<<20, 0.0005*out.Capacity)
	fc := extrapolate(out, f, minimum, FullFloor(*maxBytes))
	return &fc
}

func forecastInodes(latest Reading, f fit, opts Options) Forecast {
	out := Forecast{Measure: MeasureInodes, Target: TargetFilesystem, AsOf: latest.At}
	if !latest.HasInodes {
		out.State = StateNoData
		return out
	}
	out.Consumed = float64(latest.Inodes - latest.InodesFree)
	out.Capacity, out.Headroom = float64(latest.Inodes), float64(latest.InodesFree)
	// A growth step adds inodes on ext4 and XFS alike, but how many is
	// the filesystem's business: the line drawn stays the current count.
	out.AutoGrowSteps = autoGrowAhead(latest, opts)
	minimum := math.Max(16, 0.0005*out.Capacity)
	return extrapolate(out, f, minimum, 0)
}

// extrapolate fills the state, rate and dates of out from the fit; a
// headroom at or below fullBelow is full.
func extrapolate(out Forecast, f fit, minimum, fullBelow float64) Forecast {
	out.MinimumHours, out.HoursFound = MinHours, f.hours
	out.WindowStart, out.WindowEnd, out.WindowHours = f.start, f.end, f.hours
	out.MinimumGrowthOverWindow = minimum
	if out.Headroom <= fullBelow {
		out.State = StateFull
		return out
	}
	if f.hours < MinHours || f.end.Sub(f.start) < MinSpan {
		out.State = StateTooFewPoints
		return out
	}
	rate := f.rate
	out.FirstHalfRatePerDay, out.RecentRatePerDay = f.first*24, f.recent*24
	if f.halvesComparable && f.first > 0 && f.recent >= AccelerationRatio*f.first {
		out.Accelerating, rate = true, f.recent
	}
	if rate <= 0 || rate*f.end.Sub(f.start).Hours() < minimum {
		out.State = StateNotGrowing
		out.RatePerDay = f.rate * 24
		return out
	}
	out.State, out.RatePerDay = StateForecast, rate*24
	at := func(perHour float64) *time.Time {
		hours := (out.Headroom - fullBelow) / perHour
		if hours > 100*365*24 {
			hours = 100 * 365 * 24 // a century: "not in any horizon that matters"
		}
		t := out.AsOf.Add(time.Duration(hours * float64(time.Hour)))
		return &t
	}
	out.FullAt = at(rate)
	if f.high > 0 {
		out.EarliestAt = at(math.Max(f.high, rate))
	}
	if f.low > 0 {
		out.LatestAt = at(math.Min(f.low, rate))
	}
	spread := math.Inf(1)
	if f.rate > 0 {
		spread = (f.high - f.low) / f.rate
	}
	switch {
	case f.low > 0 && spread < 0.5 && f.hours >= 72 && !out.Accelerating:
		out.Confidence = ConfidenceHigh
	case f.low > 0 && spread < 1.5:
		out.Confidence = ConfidenceMedium
	default:
		out.Confidence = ConfidenceLow
	}
	return out
}
