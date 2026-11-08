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

package docsbundle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// BM25 parameters: the usual defaults. Documentation chunks are short and
// uniform, so the length normalisation is the standard one.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// A term in a section heading counts this many times in that chunk.
	headingBoost = 3
	// Query weight of a compound or CamelCase identifier taken whole.
	wholeWeight = 2.0
)

const indexMagic = "PGTBDOC1\n"

type pageMeta struct {
	Path  string // served path: /docs/console/tour/
	Title string
	File  string // under pages/
}

type chunkMeta struct {
	Page    int
	ID      string
	Heading string
	Start   int // byte offsets of the chunk in the page file
	End     int
	Length  int // weighted token count (the BM25 document length)
}

type posting struct {
	Chunk int
	TF    int
}

// index is the whole search structure; it is built once (make docs-bundle),
// written as index.bin and decoded at start.
type index struct {
	Version  string
	Pages    []pageMeta
	Chunks   []chunkMeta
	Terms    map[string][]posting
	TotalLen int
}

// termCounts counts the tokens of one chunk: the body once, the section
// heading headingBoost times, the page title once.
func termCounts(title, heading, body string) (map[string]int, int) {
	counts := map[string]int{}
	length := 0
	add := func(text string, weight int) {
		for _, t := range tokenize(text) {
			counts[t] += weight
			length += weight
		}
	}
	add(body, 1)
	add(heading, headingBoost)
	add(title, 1)
	return counts, length
}

func (ix *index) addChunk(c chunkMeta, counts map[string]int) {
	id := len(ix.Chunks)
	ix.Chunks = append(ix.Chunks, c)
	ix.TotalLen += c.Length
	for term, tf := range counts {
		ix.Terms[term] = append(ix.Terms[term], posting{Chunk: id, TF: tf})
	}
}

// scored is a chunk and its BM25 score before it becomes a Hit.
type scored struct {
	chunk int
	score float64
}

// search ranks chunks for the query by BM25; ties break on chunk order so the
// result never depends on map iteration.
func (ix *index) search(query string, limit int) []scored {
	n := len(ix.Chunks)
	if n == 0 || ix.TotalLen == 0 {
		return nil
	}
	avg := float64(ix.TotalLen) / float64(n)
	seen := map[string]struct{}{}
	scores := map[int]float64{}
	for _, qt := range scan(query) {
		if _, dup := seen[qt.term]; dup {
			continue
		}
		seen[qt.term] = struct{}{}
		// An identifier written whole in the query counts double: the
		// reader means that name, not its words.
		weight := 1.0
		if qt.whole {
			weight = wholeWeight
		}
		posts := ix.Terms[qt.term]
		if len(posts) == 0 {
			continue
		}
		df := float64(len(posts))
		idf := math.Log(1 + (float64(n)-df+0.5)/(df+0.5))
		for _, p := range posts {
			tf := float64(p.TF)
			dl := float64(ix.Chunks[p.Chunk].Length)
			scores[p.Chunk] += weight * idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avg))
		}
	}
	out := make([]scored, 0, len(scores))
	for c, s := range scores {
		out = append(out, scored{c, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].chunk < out[j].chunk
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// encode writes the index as varints and length-prefixed strings. Terms are
// sorted and postings are in chunk order with the chunk numbers delta-coded,
// so the same index is always the same bytes.
func (ix *index) encode() []byte {
	var b bytes.Buffer
	b.WriteString(indexMagic)
	putS := func(s string) {
		putI(&b, len(s))
		b.WriteString(s)
	}
	putS(ix.Version)
	putI(&b, ix.TotalLen)
	putI(&b, len(ix.Pages))
	for _, p := range ix.Pages {
		putS(p.Path)
		putS(p.Title)
		putS(p.File)
	}
	putI(&b, len(ix.Chunks))
	for _, c := range ix.Chunks {
		putI(&b, c.Page)
		putS(c.ID)
		putS(c.Heading)
		putI(&b, c.Start)
		putI(&b, c.End-c.Start)
		putI(&b, c.Length)
	}
	terms := make([]string, 0, len(ix.Terms))
	for t := range ix.Terms {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	putI(&b, len(terms))
	for _, t := range terms {
		putS(t)
		posts := ix.Terms[t]
		sort.Slice(posts, func(i, j int) bool { return posts[i].Chunk < posts[j].Chunk })
		putI(&b, len(posts))
		prev := 0
		for _, p := range posts {
			putI(&b, p.Chunk-prev)
			putI(&b, p.TF)
			prev = p.Chunk
		}
	}
	return b.Bytes()
}

// putI writes a non-negative int (a length, an offset or a count).
func putI(b *bytes.Buffer, v int) {
	putU(b, uint64(v)) //nolint:gosec // never negative
}

func putU(b *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	b.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) u() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errors.New("truncated varint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) int() int {
	v := r.u()
	if v > math.MaxInt32 {
		r.err = errors.New("value out of range")
		return 0
	}
	return int(v)
}

func (r *reader) s() string {
	n := r.int()
	if r.err != nil {
		return ""
	}
	if n > len(r.b) {
		r.err = errors.New("truncated string")
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func decodeIndex(data []byte) (*index, error) {
	if !bytes.HasPrefix(data, []byte(indexMagic)) {
		return nil, errors.New("not a docs bundle index")
	}
	r := &reader{b: data[len(indexMagic):]}
	ix := &index{Terms: map[string][]posting{}}
	ix.Version = r.s()
	ix.TotalLen = r.int()
	for i, n := 0, r.int(); i < n && r.err == nil; i++ {
		ix.Pages = append(ix.Pages, pageMeta{Path: r.s(), Title: r.s(), File: r.s()})
	}
	for i, n := 0, r.int(); i < n && r.err == nil; i++ {
		c := chunkMeta{Page: r.int(), ID: r.s(), Heading: r.s(), Start: r.int()}
		c.End = c.Start + r.int()
		c.Length = r.int()
		if c.Page >= len(ix.Pages) {
			return nil, fmt.Errorf("chunk %q names page %d of %d", c.ID, c.Page, len(ix.Pages))
		}
		ix.Chunks = append(ix.Chunks, c)
	}
	for i, n := 0, r.int(); i < n && r.err == nil; i++ {
		term := r.s()
		posts := make([]posting, 0, 4)
		chunk := 0
		for j, m := 0, r.int(); j < m && r.err == nil; j++ {
			chunk += r.int()
			if chunk >= len(ix.Chunks) {
				return nil, fmt.Errorf("term %q names chunk %d of %d", term, chunk, len(ix.Chunks))
			}
			posts = append(posts, posting{Chunk: chunk, TF: r.int()})
		}
		ix.Terms[term] = posts
	}
	if r.err != nil {
		return nil, fmt.Errorf("docs bundle index: %w", r.err)
	}
	if len(r.b) != 0 {
		return nil, errors.New("docs bundle index: trailing bytes")
	}
	return ix, nil
}
