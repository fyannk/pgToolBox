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

package topology

import (
	"fmt"
	"strings"
)

// The drawing's orthogonal routes. Every wire is a polyline of stated
// waypoints — the layout decides where it runs — and this file only
// cleans the list and renders it with rounded elbows.

// cornerRadius rounds each elbow of a route.
const cornerRadius = 6.0

// point is one waypoint of a route, in SVG coordinates.
type point struct{ x, y float64 }

// corners reduces a route to its turns: repeated vertices and collinear
// midpoints do not survive, so an aligned route collapses to a straight
// line.
func corners(in []point) []point {
	var distinct []point
	for _, p := range in {
		if len(distinct) > 0 && sameSpot(distinct[len(distinct)-1], p) {
			continue
		}
		distinct = append(distinct, p)
	}
	var out []point
	for i, p := range distinct {
		if i == 0 || i == len(distinct)-1 {
			out = append(out, p)
			continue
		}
		before, after := distinct[i-1], distinct[i+1]
		straight := (closeTo(before.x, p.x) && closeTo(p.x, after.x)) ||
			(closeTo(before.y, p.y) && closeTo(p.y, after.y))
		if !straight {
			out = append(out, p)
		}
	}
	return out
}

func sameSpot(a, b point) bool { return closeTo(a.x, b.x) && closeTo(a.y, b.y) }

func closeTo(a, b float64) bool {
	d := a - b
	return d < 0.6 && d > -0.6
}

// roundedRoute renders a polyline as SVG path data (M, L and Q
// commands, each followed by a space) with rounded elbows, shrinking a
// corner that would not fit its segment.
func roundedRoute(points []point) string {
	var b strings.Builder
	fmt.Fprintf(&b, "M %s %s", coord(points[0].x), coord(points[0].y))
	for i := 1; i < len(points); i++ {
		p := points[i]
		if i == len(points)-1 {
			fmt.Fprintf(&b, " L %s %s", coord(p.x), coord(p.y))
			continue
		}
		before, after := points[i-1], points[i+1]
		radius := cornerRadius
		if half := span(before, p) / 2; half < radius {
			radius = half
		}
		if half := span(p, after) / 2; half < radius {
			radius = half
		}
		inX, inY := direction(p.x-before.x), direction(p.y-before.y)
		outX, outY := direction(after.x-p.x), direction(after.y-p.y)
		fmt.Fprintf(&b, " L %s %s", coord(p.x-inX*radius), coord(p.y-inY*radius))
		fmt.Fprintf(&b, " Q %s %s %s %s", coord(p.x), coord(p.y),
			coord(p.x+outX*radius), coord(p.y+outY*radius))
	}
	return b.String()
}

// span is the axis-aligned distance between two waypoints.
func span(a, b point) float64 {
	dx, dy := a.x-b.x, a.y-b.y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return dx + dy
}

func direction(v float64) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// coord renders a coordinate without a pointless trailing zero.
func coord(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}
