// Package diff computes line diffs and formats them as unified diffs.
//
// Lines compares two lists of lines with patience diff: lines that occur
// exactly once on both sides anchor the comparison, and the regions between
// anchors are compared again. A region without such lines is compared with
// Myers' algorithm, which finds a shortest edit script, in linear space.
// Patience diff keeps unrelated changes apart on the unique lines of code,
// such as declarations, and bounds the work on large files: a region too
// large for Myers' algorithm is replaced as a whole.
package diff

import (
	"fmt"
	"strings"
)

// Op is the kind of an edit.
type Op int8

const (
	// Equal keeps a line of both sides.
	Equal Op = iota
	// Delete removes a line of the old side.
	Delete
	// Insert adds a line of the new side.
	Insert
)

// Edit is one line of an edit script. A is the index of the line in the old
// side for Equal and Delete, B its index in the new side for Equal and
// Insert; the other index is the position the line is at.
type Edit struct {
	Op   Op
	A, B int
}

// maxMyers is the largest combined length of a region compared with Myers'
// algorithm, whose cost grows with the product of the length and the number
// of differences. Larger regions without anchors are replaced whole.
const maxMyers = 6000

// Lines returns an edit script that turns a into b.
func Lines(a, b []string) []Edit {
	// Lines are compared by number, which makes comparisons cheap.
	ids := map[string]int{}
	intern := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	d := &differ{a: intern(a), b: intern(b)}
	d.diff(0, len(a), 0, len(b))
	return d.edits
}

type differ struct {
	a, b  []int
	edits []Edit
}

func (d *differ) equal(alo, blo, n int) {
	for i := range n {
		d.edits = append(d.edits, Edit{Op: Equal, A: alo + i, B: blo + i})
	}
}

// replace deletes a[alo:ahi] and inserts b[blo:bhi].
func (d *differ) replace(alo, ahi, blo, bhi int) {
	for i := alo; i < ahi; i++ {
		d.edits = append(d.edits, Edit{Op: Delete, A: i, B: blo})
	}
	for j := blo; j < bhi; j++ {
		d.edits = append(d.edits, Edit{Op: Insert, A: ahi, B: j})
	}
}

// trim returns the lengths of the common prefix and suffix of the ranges.
func (d *differ) trim(alo, ahi, blo, bhi int) (prefix, suffix int) {
	for alo+prefix < ahi && blo+prefix < bhi && d.a[alo+prefix] == d.b[blo+prefix] {
		prefix++
	}
	for alo+prefix < ahi-suffix && blo+prefix < bhi-suffix && d.a[ahi-suffix-1] == d.b[bhi-suffix-1] {
		suffix++
	}
	return prefix, suffix
}

// diff compares a[alo:ahi] with b[blo:bhi] by patience diff.
func (d *differ) diff(alo, ahi, blo, bhi int) {
	prefix, suffix := d.trim(alo, ahi, blo, bhi)
	d.equal(alo, blo, prefix)
	alo, blo = alo+prefix, blo+prefix
	ahi, bhi = ahi-suffix, bhi-suffix
	switch anchors := d.anchors(alo, ahi, blo, bhi); {
	case alo == ahi || blo == bhi:
		d.replace(alo, ahi, blo, bhi)
	case len(anchors) > 0:
		for _, m := range anchors {
			d.diff(alo, m[0], blo, m[1])
			d.equal(m[0], m[1], 1)
			alo, blo = m[0]+1, m[1]+1
		}
		d.diff(alo, ahi, blo, bhi)
	case ahi-alo+bhi-blo <= maxMyers:
		d.myers(alo, ahi, blo, bhi)
	default:
		d.replace(alo, ahi, blo, bhi)
	}
	d.equal(ahi, bhi, suffix)
}

// anchors returns the longest increasing sequence of the line pairs that
// occur exactly once in each range, as positions in a and b.
func (d *differ) anchors(alo, ahi, blo, bhi int) [][2]int {
	type count struct{ a, b, posA, posB int }
	counts := map[int]*count{}
	for i := alo; i < ahi; i++ {
		c := counts[d.a[i]]
		if c == nil {
			c = &count{}
			counts[d.a[i]] = c
		}
		c.a++
		c.posA = i
	}
	for j := blo; j < bhi; j++ {
		if c := counts[d.b[j]]; c != nil {
			c.b++
			c.posB = j
		}
	}
	// Unique pairs in the order of a.
	var pairs [][2]int
	for i := alo; i < ahi; i++ {
		if c := counts[d.a[i]]; c.a == 1 && c.b == 1 {
			pairs = append(pairs, [2]int{i, c.posB})
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	// Patience sorting: tails[k] is the pair ending the best increasing
	// sequence of length k+1 found so far; prev links each pair to its
	// predecessor in that sequence.
	var tails []int
	prev := make([]int, len(pairs))
	for i, p := range pairs {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if pairs[tails[mid]][1] < p[1] {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[i] = -1
		if lo > 0 {
			prev[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	seq := make([][2]int, len(tails))
	for i, k := len(tails)-1, tails[len(tails)-1]; i >= 0; i, k = i-1, prev[k] {
		seq[i] = pairs[k]
	}
	return seq
}

// myers compares a[alo:ahi] with b[blo:bhi] with Myers' algorithm, splitting
// the ranges at a point on a shortest edit path until they are trivial.
func (d *differ) myers(alo, ahi, blo, bhi int) {
	prefix, suffix := d.trim(alo, ahi, blo, bhi)
	d.equal(alo, blo, prefix)
	alo, blo = alo+prefix, blo+prefix
	ahi, bhi = ahi-suffix, bhi-suffix
	if alo == ahi || blo == bhi {
		d.replace(alo, ahi, blo, bhi)
	} else if x, y, ok := bisect(d.a[alo:ahi], d.b[blo:bhi]); ok {
		d.myers(alo, alo+x, blo, blo+y)
		d.myers(alo+x, ahi, blo+y, bhi)
	} else {
		d.replace(alo, ahi, blo, bhi)
	}
	d.equal(ahi, bhi, suffix)
}

// bisect finds the middle snake of a shortest edit path from a to b by
// running the search from both ends at once, as in Myers' linear-space
// refinement, and returns the point where the two searches meet. The first
// and last lines of a and b must differ, which keeps the point strictly
// inside the ranges.
func bisect(a, b []int) (x, y int, ok bool) {
	n, m := len(a), len(b)
	maxD := (n + m + 1) / 2
	off := maxD
	// Two spare entries hold the start at off+1 when maxD is 1.
	v1 := make([]int, 2*maxD+2)
	v2 := make([]int, 2*maxD+2)
	for i := range v1 {
		v1[i], v2[i] = -1, -1
	}
	v1[off+1], v2[off+1] = 0, 0
	delta := n - m
	// With an odd delta the forward search finds the overlap; with an even
	// one, the reverse search.
	front := delta%2 != 0
	// The searches skip diagonals that ran off the edges.
	k1start, k1end, k2start, k2end := 0, 0, 0, 0
	for step := range maxD {
		for k1 := -step + k1start; k1 <= step-k1end; k1 += 2 {
			i := off + k1
			var x1 int
			if k1 == -step || (k1 != step && v1[i-1] < v1[i+1]) {
				x1 = v1[i+1]
			} else {
				x1 = v1[i-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < m && a[x1] == b[y1] {
				x1++
				y1++
			}
			v1[i] = x1
			switch {
			case x1 > n:
				k1end += 2
			case y1 > m:
				k1start += 2
			case front:
				if j := off + delta - k1; j >= 0 && j < len(v2) && v2[j] != -1 && x1 >= n-v2[j] {
					return split(x1, y1, n, m)
				}
			}
		}
		for k2 := -step + k2start; k2 <= step-k2end; k2 += 2 {
			i := off + k2
			var x2 int
			if k2 == -step || (k2 != step && v2[i-1] < v2[i+1]) {
				x2 = v2[i+1]
			} else {
				x2 = v2[i-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < m && a[n-x2-1] == b[m-y2-1] {
				x2++
				y2++
			}
			v2[i] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > m:
				k2start += 2
			case !front:
				if j := off + delta - k2; j >= 0 && j < len(v1) && v1[j] != -1 {
					x1 := v1[j]
					if y1 := off + x1 - j; x1 >= n-x2 {
						return split(x1, y1, n, m)
					}
				}
			}
		}
	}
	return 0, 0, false
}

// split returns a meeting point unless it is a corner, which would not
// make the ranges smaller.
func split(x, y, n, m int) (int, int, bool) {
	if (x == 0 && y == 0) || (x == n && y == m) {
		return 0, 0, false
	}
	return x, y, true
}

// SplitLines splits text into lines that keep their "\n" terminator; the
// last line has none if the text does not end with a newline.
func SplitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Unified returns the unified diff that turns old into new, with up to
// context unchanged lines around each change, or "" if they are equal. The
// header names the sides oldName and newName, for example "a/main.go" and
// "b/main.go", or "/dev/null" for a side that does not exist. A last line
// without a newline is marked as in diff and git.
func Unified(oldName, newName, old, new string, context int) string {
	if old == new {
		return ""
	}
	a, b := SplitLines(old), SplitLines(new)
	edits := Lines(a, b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldName, newName)
	for _, h := range hunks(edits, max(context, 0)) {
		writeHunk(&sb, a, b, edits[h[0]:h[1]])
	}
	return sb.String()
}

// hunks groups the edits into ranges of changes with their context. Changes
// separated by at most 2*context unchanged lines share a hunk.
func hunks(edits []Edit, context int) [][2]int {
	var out [][2]int
	start, end := -1, -1 // the current hunk, end after its last change
	for i, e := range edits {
		if e.Op == Equal {
			continue
		}
		if start >= 0 && i-end > 2*context {
			out = append(out, [2]int{start, min(end+context, len(edits))})
			start = -1
		}
		if start < 0 {
			start = max(i-context, 0)
		}
		end = i + 1
	}
	if start >= 0 {
		out = append(out, [2]int{start, min(end+context, len(edits))})
	}
	return out
}

func writeHunk(sb *strings.Builder, a, b []string, edits []Edit) {
	var na, nb int
	for _, e := range edits {
		switch e.Op {
		case Equal:
			na++
			nb++
		case Delete:
			na++
		case Insert:
			nb++
		}
	}
	fmt.Fprintf(sb, "@@ -%s +%s @@\n", hunkRange(edits[0].A, na), hunkRange(edits[0].B, nb))
	for _, e := range edits {
		switch e.Op {
		case Equal:
			writeLine(sb, ' ', a[e.A])
		case Delete:
			writeLine(sb, '-', a[e.A])
		case Insert:
			writeLine(sb, '+', b[e.B])
		}
	}
}

// hunkRange formats the line range of a hunk side starting at the 0-based
// index start: an empty range names the line before it, and a count of one
// is omitted.
func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func writeLine(sb *strings.Builder, prefix byte, line string) {
	sb.WriteByte(prefix)
	text, ok := strings.CutSuffix(line, "\n")
	text = strings.TrimSuffix(text, "\r")
	sb.WriteString(text)
	sb.WriteByte('\n')
	if !ok {
		sb.WriteString("\\ No newline at end of file\n")
	}
}
