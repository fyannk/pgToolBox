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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Check lists the drawing's violations of the rules every layout keeps,
// so the read model, the API fixture and this package's tests hold the
// drawing to one set: every box, frame, dot and label inside the drawing;
// box text inside its box; frames that hold their heading and overlap no
// other frame; boxes that overlap no other box and sit in at most one
// frame; edges that start and end on a box border (or, for a schedule's
// drop, on a frame's top line); labels clear of every box. Text is
// measured at charW px a character, 12 px high; a label is centred on its
// anchor, and an upright one is turned a quarter left.
func Check(t viewmodel.Topology) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	inside := func(x0, y0, x1, y1 int) bool { return x0 >= 0 && y0 >= 0 && x1 <= t.Width && y1 <= t.Height }
	overlap := func(ax0, ay0, ax1, ay1, bx0, by0, bx1, by1 int) bool {
		return ax0 < bx1 && bx0 < ax1 && ay0 < by1 && by0 < ay1
	}

	for i, n := range t.Nodes {
		if !inside(n.X, n.Y, n.X+n.W, n.Y+n.H) {
			bad("node %s outside the %dx%d drawing", n.ID, t.Width, t.Height)
		}
		if len(n.Lines) == 0 {
			bad("node %s has no lines", n.ID)
		}
		for _, l := range n.Lines {
			if l.Y < n.Y+12 || l.Y > n.Y+n.H-8 || l.X < n.X || l.X+charW*utf8.RuneCountInString(l.Text) > n.X+n.W {
				bad("node %s line %q outside its box", n.ID, l.Text)
			}
		}
		for _, m := range t.Nodes[i+1:] {
			if overlap(n.X, n.Y, n.X+n.W, n.Y+n.H, m.X, m.Y, m.X+m.W, m.Y+m.H) {
				bad("node %s overlaps node %s", n.ID, m.ID)
			}
		}
		holders := 0
		for _, f := range t.Frames {
			if overlap(n.X, n.Y, n.X+n.W, n.Y+n.H, f.X, f.Y, f.X+f.W, f.Y+f.H) {
				holders++
				if n.X < f.X || n.Y < f.Y || n.X+n.W > f.X+f.W || n.Y+n.H > f.Y+f.H {
					bad("node %s straddles frame %q", n.ID, f.Label)
				}
			}
		}
		if holders > 1 {
			bad("node %s sits in %d frames", n.ID, holders)
		}
	}

	for i, f := range t.Frames {
		if !inside(f.X, f.Y, f.X+f.W, f.Y+f.H) {
			bad("frame %q outside the %dx%d drawing", f.Label, t.Width, t.Height)
		}
		if HeadingWidth(f.Label, f.Note) > f.W {
			bad("frame %q heading overflows its frame", f.Label)
		}
		for _, g := range t.Frames[i+1:] {
			if overlap(f.X, f.Y, f.X+f.W, f.Y+f.H, g.X, g.Y, g.X+g.W, g.Y+g.H) {
				bad("frame %q overlaps frame %q", f.Label, g.Label)
			}
		}
	}

	onBorder := func(x, y int) bool {
		for _, n := range t.Nodes {
			if (x == n.X || x == n.X+n.W) && y >= n.Y && y <= n.Y+n.H {
				return true
			}
			if (y == n.Y || y == n.Y+n.H) && x >= n.X && x <= n.X+n.W {
				return true
			}
		}
		return false
	}
	onFrameTop := func(x, y int) bool {
		for _, f := range t.Frames {
			if y == f.Y && x >= f.X && x <= f.X+f.W {
				return true
			}
		}
		return false
	}
	for _, e := range t.Edges {
		first, last, ok := ends(e.Path)
		if !ok {
			bad("edge %s path %q is not M ... L x y", e.Kind, e.Path)
			continue
		}
		if !onBorder(first[0], first[1]) {
			bad("edge %s starts at %v, off every box border", e.Kind, first)
		}
		if !onBorder(last[0], last[1]) && !(e.Kind == EdgeBaseBackup && onFrameTop(last[0], last[1])) {
			bad("edge %s ends at %v, off every box border", e.Kind, last)
		}
		if e.Label == "" {
			continue
		}
		half := charW * utf8.RuneCountInString(e.Label) / 2
		x0, y0, x1, y1 := e.LabelX-half, e.LabelY-12, e.LabelX+half, e.LabelY
		if e.LabelVertical {
			x0, y0, x1, y1 = e.LabelX-12, e.LabelY-half, e.LabelX+3, e.LabelY+half
		}
		if !inside(x0, y0, x1, y1) {
			bad("label %q outside the %dx%d drawing", e.Label, t.Width, t.Height)
		}
		for _, n := range t.Nodes {
			if overlap(x0, y0, x1, y1, n.X, n.Y, n.X+n.W, n.Y+n.H) {
				bad("label %q overlaps node %s", e.Label, n.ID)
			}
		}
	}
	for _, d := range t.Dots {
		if !inside(d.X, d.Y, d.X, d.Y) {
			bad("dot %s at %d,%d outside the drawing", d.Kind, d.X, d.Y)
		}
	}
	return out
}

// ends reads the first and last points of a path this package rendered,
// rounded to whole units.
func ends(path string) (first, last [2]int, ok bool) {
	f := strings.Fields(path)
	if len(f) < 6 || f[0] != "M" || f[len(f)-3] != "L" {
		return first, last, false
	}
	num := func(s string) (int, bool) {
		v, err := strconv.ParseFloat(s, 64)
		return int(v), err == nil
	}
	var good [4]bool
	first[0], good[0] = num(f[1])
	first[1], good[1] = num(f[2])
	last[0], good[2] = num(f[len(f)-2])
	last[1], good[3] = num(f[len(f)-1])
	return first, last, good[0] && good[1] && good[2] && good[3]
}
