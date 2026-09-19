package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Point is [lng, lat] in EPSG:4326. The rescue scene is local enough that
// planar math on equirectangular coordinates is adequate for conflict checks;
// conflict points returned to clients stay in lng/lat.
type Point struct {
	Lng float64 `json:"lng"`
	Lat float64 `json:"lat"`
}

// Ring is a closed simple polygon exterior (no holes in this service).
type Ring []Point

// Geom is one or more independent rings (Polygon or MultiPolygon on the wire).
type Geom struct {
	Rings []Ring
}

type geoJSON struct {
	Type        string        `json:"type"`
	Coordinates []interface{} `json:"coordinates"`
}

func (p Point) isZero() bool { return p.Lng == 0 && p.Lat == 0 }

// ParseGeom accepts a GeoJSON Polygon or MultiPolygon geometry object.
func ParseGeom(raw json.RawMessage) (*Geom, error) {
	if len(raw) == 0 {
		return nil, errors.New("geometry is required")
	}
	var gj geoJSON
	if err := json.Unmarshal(raw, &gj); err != nil {
		return nil, fmt.Errorf("invalid GeoJSON: %w", err)
	}
	g := &Geom{}
	switch gj.Type {
	case "Polygon":
		r, err := parsePolygonCoords(gj.Coordinates)
		if err != nil {
			return nil, err
		}
		g.Rings = []Ring{r}
	case "MultiPolygon":
		if len(gj.Coordinates) == 0 {
			return nil, errors.New("MultiPolygon has no polygons")
		}
		for i, poly := range gj.Coordinates {
			arr, ok := poly.([]interface{})
			if !ok {
				return nil, fmt.Errorf("MultiPolygon[%d] malformed", i)
			}
			r, err := parsePolygonCoords(arr)
			if err != nil {
				return nil, fmt.Errorf("MultiPolygon[%d]: %w", i, err)
			}
			g.Rings = append(g.Rings, r)
		}
	default:
		return nil, fmt.Errorf("unsupported geometry type %q (need Polygon or MultiPolygon)", gj.Type)
	}
	for i, r := range g.Rings {
		if err := ValidateRing(r); err != nil {
			return nil, fmt.Errorf("ring %d: %w", i, err)
		}
	}
	return g, nil
}

func parsePolygonCoords(in []interface{}) (Ring, error) {
	if len(in) == 0 {
		return nil, errors.New("polygon has no rings")
	}
	if len(in) > 1 {
		return nil, fmt.Errorf("polygon with %d rings: holes are not supported", len(in))
	}
	ringArr, ok := in[0].([]interface{})
	if !ok {
		return nil, errors.New("exterior ring malformed")
	}
	if len(ringArr) < 4 {
		return nil, errors.New("polygon needs at least 4 positions (closed ring)")
	}
	r := make(Ring, 0, len(ringArr))
	for j, pos := range ringArr {
		coord, ok := pos.([]interface{})
		if !ok || len(coord) < 2 {
			return nil, fmt.Errorf("position %d malformed", j)
		}
		lng, ok1 := coord[0].(float64)
		lat, ok2 := coord[1].(float64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("position %d coordinates must be numbers", j)
		}
		r = append(r, Point{Lng: lng, Lat: lat})
	}
	return r, nil
}

// Marshal emits Polygon for a single ring, MultiPolygon otherwise.
func (g *Geom) Marshal() json.RawMessage {
	if g == nil {
		return nil
	}
	if len(g.Rings) == 1 {
		b, _ := json.Marshal(geoJSON{Type: "Polygon", Coordinates: ringToWire(g.Rings[0])})
		return b
	}
	polys := make([]interface{}, 0, len(g.Rings))
	for _, r := range g.Rings {
		polys = append(polys, ringToWire(r))
	}
	b, _ := json.Marshal(geoJSON{Type: "MultiPolygon", Coordinates: polys})
	return b
}

func ringToWire(r Ring) []interface{} {
	ring := make([]interface{}, len(r))
	for i, p := range r {
		ring[i] = []float64{p.Lng, p.Lat}
	}
	return []interface{}{ring}
}

// ValidateRing enforces a closed, simple (non-self-intersecting) exterior.
func ValidateRing(r Ring) error {
	if len(r) < 4 {
		return errors.New("ring needs at least 4 positions")
	}
	if r[0] != r[len(r)-1] {
		return errors.New("ring is not closed (first and last position must match)")
	}
	if math.Abs(signedArea(r)) < 1e-12 {
		return errors.New("polygon area is zero")
	}
	n := len(r) - 1 // edge count of closed ring
	for i := 0; i < n; i++ {
		a, b := r[i], r[(i+1)%n]
		if a == b {
			return fmt.Errorf("zero-length edge at position %d", i)
		}
	}
	// Non-adjacent edges must not cross (proper intersection or overlap).
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if i == j || (i+1)%n == j || (j+1)%n == i {
				continue
			}
			p1, p2 := r[i], r[(i+1)%n]
			p3, p4 := r[j], r[(j+1)%n]
			if properCross(p1, p2, p3, p4) {
				return fmt.Errorf("ring self-intersects at edges %d and %d", i, j)
			}
		}
	}
	return nil
}

func signedArea(r Ring) float64 {
	a := 0.0
	n := len(r)
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		a += r[i].Lng*r[j].Lat - r[j].Lng*r[i].Lat
	}
	return a / 2
}

func orientCCW(r Ring) Ring {
	if signedArea(r) < 0 {
		out := make(Ring, len(r))
		for i, p := range r {
			out[len(r)-1-i] = p
		}
		return out
	}
	return r
}

const eps = 1e-9

func cross2(o, a, b Point) float64 {
	return (a.Lng-o.Lng)*(b.Lat-o.Lat) - (a.Lat-o.Lat)*(b.Lng-o.Lng)
}

func onSeg(p, a, b Point) bool {
	return math.Abs(cross2(a, b, p)) < eps &&
		p.Lng >= math.Min(a.Lng, b.Lng)-eps && p.Lng <= math.Max(a.Lng, b.Lng)+eps &&
		p.Lat >= math.Min(a.Lat, b.Lat)-eps && p.Lat <= math.Max(a.Lat, b.Lat)+eps
}

// properCross reports an intersection strictly in the interiors of both
// segments (endpoints touch is allowed — adjacent ring edges share endpoints).
func properCross(p1, p2, p3, p4 Point) bool {
	d1 := cross2(p3, p4, p1)
	d2 := cross2(p3, p4, p2)
	d3 := cross2(p1, p2, p3)
	d4 := cross2(p1, p2, p4)
	if ((d1 > eps && d2 < -eps) || (d1 < -eps && d2 > eps)) &&
		((d3 > eps && d4 < -eps) || (d3 < -eps && d4 > eps)) {
		return true
	}
	// Collinear overlap is invalid too.
	if math.Abs(d1) < eps && onSegInterior(p1, p3, p4) {
		return true
	}
	if math.Abs(d2) < eps && onSegInterior(p2, p3, p4) {
		return true
	}
	if math.Abs(d3) < eps && onSegInterior(p3, p1, p2) {
		return true
	}
	if math.Abs(d4) < eps && onSegInterior(p4, p1, p2) {
		return true
	}
	return false
}

func onSegInterior(p, a, b Point) bool {
	if !onSeg(p, a, b) {
		return false
	}
	dx, dy := b.Lng-a.Lng, b.Lat-a.Lat
	t := 0.0
	if math.Abs(dx) > math.Abs(dy) {
		t = (p.Lng - a.Lng) / dx
	} else if dy != 0 {
		t = (p.Lat - a.Lat) / dy
	}
	return t > eps && t < 1-eps
}

func segIntersectionPoint(p1, p2, p3, p4 Point) (Point, bool) {
	x1, y1, x2, y2 := p1.Lng, p1.Lat, p2.Lng, p2.Lat
	x3, y3, x4, y4 := p3.Lng, p3.Lat, p4.Lng, p4.Lat
	d := (x1-x2)*(y3-y4) - (y1-y2)*(x3-x4)
	if math.Abs(d) < eps {
		return Point{}, false
	}
	t := ((x1-x3)*(y3-y4) - (y1-y3)*(x3-x4)) / d
	u := -((x1-x2)*(y1-y3) - (y1-y2)*(x1-x3)) / d
	if t < -eps || t > 1+eps || u < -eps || u > 1+eps {
		return Point{}, false
	}
	return Point{Lng: x1 + t*(x2-x1), Lat: y1 + t*(y2-y1)}, true
}

// bbox quick reject
type bbox struct{ minLng, minLat, maxLng, maxLat float64 }

func ringBBox(r Ring) bbox {
	b := bbox{minLng: r[0].Lng, maxLng: r[0].Lng, minLat: r[0].Lat, maxLat: r[0].Lat}
	for _, p := range r[1:] {
		b.minLng = math.Min(b.minLng, p.Lng)
		b.maxLng = math.Max(b.maxLng, p.Lng)
		b.minLat = math.Min(b.minLat, p.Lat)
		b.maxLat = math.Max(b.maxLat, p.Lat)
	}
	return b
}

func (b bbox) overlaps(o bbox) bool {
	return b.minLng <= o.maxLng+eps && b.maxLng >= o.minLng-eps &&
		b.minLat <= o.maxLat+eps && b.maxLat >= o.minLat-eps
}

func pointInRing(p Point, r Ring) bool {
	inside := false
	n := len(r) - 1
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		pi, pj := r[i], r[j]
		if onSeg(p, pi, pj) {
			return true
		}
		if (pi.Lat > p.Lat) != (pj.Lat > p.Lat) {
			x := pi.Lng + (pj.Lng-pi.Lng)*(p.Lat-pi.Lat)/(pj.Lat-pi.Lat)
			if x > p.Lng {
				inside = !inside
			}
		}
	}
	return inside
}

// GeomContainsPoint reports p inside or on the boundary of g.
func GeomContainsPoint(g *Geom, p Point) bool {
	for _, r := range g.Rings {
		if pointInRing(p, r) {
			return true
		}
	}
	return false
}

type ringCache struct {
	ring Ring
	box  bbox
}

func cacheGeom(g *Geom) []ringCache {
	out := make([]ringCache, len(g.Rings))
	for i, r := range g.Rings {
		out[i] = ringCache{ring: r, box: ringBBox(r)}
	}
	return out
}

// GeomIntersects reports any boundary crossing or containment between a and b.
func GeomIntersects(a, b *Geom) bool {
	return len(IntersectionPoints(a, b)) > 0 || geomContains(a, b) || geomContains(b, a)
}

func geomContains(outer, inner *Geom) bool {
	for _, r := range inner.Rings {
		for _, p := range r {
			if !GeomContainsPoint(outer, p) {
				return false
			}
		}
	}
	return true
}

// IntersectionPoints returns boundary crossings plus vertices of b lying in a
// (covers total containment), de-duplicated. These are the "空间冲突位置".
func IntersectionPoints(a, b *Geom) []Point {
	ca, cb := cacheGeom(a), cacheGeom(b)
	var pts []Point
	for _, ra := range ca {
		for _, rb := range cb {
			if !ra.box.overlaps(rb.box) {
				continue
			}
			na, nb := len(ra.ring)-1, len(rb.ring)-1
			for i := 0; i < na; i++ {
				p1, p2 := ra.ring[i], ra.ring[(i+1)%na]
				for j := 0; j < nb; j++ {
					p3, p4 := rb.ring[j], rb.ring[(j+1)%nb]
					if p, ok := segIntersectionPoint(p1, p2, p3, p4); ok {
						pts = append(pts, p)
					}
				}
			}
			// Touching / contained vertices anchor the conflict location too.
			for _, p := range rb.ring[:nb] {
				if pointInRing(p, ra.ring) {
					pts = append(pts, p)
				}
			}
			for _, p := range ra.ring[:na] {
				if pointInRing(p, rb.ring) {
					pts = append(pts, p)
				}
			}
		}
	}
	return dedupePoints(pts)
}

func dedupePoints(pts []Point) []Point {
	type key struct{ x, y int64 }
	seen := map[key]bool{}
	out := make([]Point, 0, len(pts))
	for _, p := range pts {
		k := key{int64(math.Round(p.Lng * 1e7)), int64(math.Round(p.Lat * 1e7))}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, Point{Lng: round7(p.Lng), Lat: round7(p.Lat)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lng != out[j].Lng {
			return out[i].Lng < out[j].Lng
		}
		return out[i].Lat < out[j].Lat
	})
	return out
}

func round7(v float64) float64 { return math.Round(v*1e7) / 1e7 }

// ---- ear-clipping triangulation, used to carve safe regions ----

type tri struct{ a, b, c int }

func triangulate(r Ring) ([]tri, error) {
	r = orientCCW(r)
	n := len(r) - 1 // exclude duplicated closing point
	if n < 3 {
		return nil, errors.New("ring too small")
	}
	idx := make([]int, n)
	for i := 0; i < n; i++ {
		idx[i] = i
	}
	var out []tri
	guard := 2 * n
	for len(idx) > 2 {
		earFound := false
		m := len(idx)
		for k := 0; k < m; k++ {
			i0, i1, i2 := idx[(k-1+m)%m], idx[k], idx[(k+1)%m]
			if !isEar(r, i0, i1, i2, idx) {
				continue
			}
			out = append(out, tri{i0, i1, i2})
			idx = append(idx[:k], idx[k+1:]...)
			earFound = true
			break
		}
		if !earFound {
			// Degenerate polygon (should be blocked by validation); fall back
			// to a fan which may overlap — callers treat results as advisory.
			for k := 1; k+1 < len(idx); k++ {
				out = append(out, tri{idx[0], idx[k], idx[k+1]})
			}
			break
		}
		guard--
		if guard < 0 {
			return nil, errors.New("triangulation failed")
		}
	}
	return out, nil
}

func isEar(r Ring, i0, i1, i2 int, idx []int) bool {
	a, b, c := r[i0], r[i1], r[i2]
	if cross2(a, b, c) <= eps {
		return false // reflex or collinear tip
	}
	for _, q := range idx {
		if q == i0 || q == i1 || q == i2 {
			continue
		}
		if pointInTriangle(r[q], a, b, c) {
			return false
		}
	}
	return true
}

func pointInTriangle(p, a, b, c Point) bool {
	d1 := cross2(a, b, p)
	d2 := cross2(b, c, p)
	d3 := cross2(c, a, p)
	hasNeg := d1 < -eps || d2 < -eps || d3 < -eps
	hasPos := d1 > eps || d2 > eps || d3 > eps
	return !(hasNeg && hasPos)
}

func triArea2(a, b, c Point) float64 { return math.Abs(cross2(a, b, c)) }

func trisIntersect(r Ring, t1, t2 tri) bool {
	p := [3]Point{r[t1.a], r[t1.b], r[t1.c]}
	q := [3]Point{r[t2.a], r[t2.b], r[t2.c]}
	// Work generically across two rings via closure over coordinates arrays.
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if properCross(p[i], p[(i+1)%3], q[j], q[(j+1)%3]) {
				return true
			}
		}
	}
	for i := 0; i < 3; i++ {
		if pointInTriangle(p[i], q[0], q[1], q[2]) {
			return true
		}
		if pointInTriangle(q[i], p[0], p[1], p[2]) {
			return true
		}
	}
	return false
}

// ---- advisory safe-area carving (adaptive grid scan + rectangle merge) ----
//
// SuggestSafe returns the portion of requested that stays clear of every
// blocker with a stand-off clearance. The advisory geometry does not need a
// perfect boolean difference: it must (1) never cross a blocker, (2) keep
// measurable separation, and (3) be simple polygons ATC can adopt as a shrink.
// A fine adaptive grid over local equirectangular coordinates satisfies all
// three; kept cells are merged into maximal rectangles.

const (
	safeGridN  = 256  // cells along the longest projected span
	safeGapM   = 35.0 // minimum stand-off from blockers (~35 m)
	mPerDegLat = 111320.0
)

type gcell struct{ ix, iy int }

func SuggestSafe(requested *Geom, blockers []*Geom) (*Geom, error) {
	if requested == nil {
		return nil, errors.New("no requested geometry")
	}
	box := geomBBox(requested)
	lat0 := (box.minLat + box.maxLat) / 2
	cosLat := math.Cos(lat0 * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	toU := func(lng float64) float64 { return (lng - box.minLng) * cosLat }
	toV := func(lat float64) float64 { return lat - box.minLat }
	toLng := func(u float64) float64 { return box.minLng + u/cosLat }

	spanU := (box.maxLng - box.minLng) * cosLat
	spanV := box.maxLat - box.minLat
	maxSpan := math.Max(spanU, spanV)
	if maxSpan <= 0 {
		return &Geom{}, nil
	}
	cell := maxSpan / safeGridN
	gap := math.Max(cell, safeGapM/mPerDegLat)

	// Expand blocker rings into projected local space for distance checks.
	type pring []struct{ u, v float64 }
	var blockRings [][][2]float64
	var blockGeoms []*Geom
	blockGeoms = append(blockGeoms, blockers...)
	_ = pring{}
	for _, bg := range blockers {
		for _, r := range bg.Rings {
			var pr [][2]float64
			for _, p := range r {
				pr = append(pr, [2]float64{toU(p.Lng), toV(p.Lat)})
			}
			blockRings = append(blockRings, pr)
		}
	}
	cellContains := func(cu, cv float64) bool {
		p := Point{Lng: toLng(cu), Lat: box.minLat + cv}
		if !GeomContainsPoint(requested, p) {
			return false
		}
		for _, bg := range blockers {
			if GeomContainsPoint(bg, p) {
				return false
			}
		}
		for _, pr := range blockRings {
			for i := 0; i < len(pr)-1; i++ {
				if pointSegDistUV(cu, cv, pr[i], pr[i+1]) < gap {
					return false
				}
			}
		}
		return true
	}

	nU := int(math.Ceil(spanU/cell)) + 1
	nV := int(math.Ceil(spanV/cell)) + 1
	kept := map[gcell]bool{}
	for iy := 0; iy < nV; iy++ {
		for ix := 0; ix < nU; ix++ {
			u0, v0 := float64(ix)*cell, float64(iy)*cell
			u1, v1 := u0+cell, v0+cell
			// Keep only when ALL four corners are inside requested and beyond
			// the stand-off; convex cells then cannot cross any blocker edge.
			if cellContains(u0, v0) && cellContains(u1, v0) &&
				cellContains(u1, v1) && cellContains(u0, v1) {
				kept[gcell{ix, iy}] = true
			}
		}
	}
	if len(kept) == 0 {
		return &Geom{}, nil
	}

	// Merge kept cells into maximal axis-aligned rectangles (greedy rows,
	// extended downward while identical and free).
	type rect struct{ ix0, iy0, ix1, iy1 int }
	var rects []rect
	used := map[gcell]bool{}
	for iy := 0; iy < nV; iy++ {
		for ix := 0; ix < nU; ix++ {
			if !kept[gcell{ix, iy}] || used[gcell{ix, iy}] {
				continue
			}
			w := 0
			for x := ix; kept[gcell{x, iy}] && !used[gcell{x, iy}]; x++ {
				w++
			}
			h := 1
			for y := iy + 1; ; y++ {
				full := true
				for x := ix; x < ix+w; x++ {
					if !kept[gcell{x, y}] || used[gcell{x, y}] {
						full = false
						break
					}
				}
				if !full {
					break
				}
				h++
			}
			for y := iy; y < iy+h; y++ {
				for x := ix; x < ix+w; x++ {
					used[gcell{x, y}] = true
				}
			}
			rects = append(rects, rect{ix, iy, ix + w, iy + h})
		}
	}

	g := &Geom{}
	for _, rc := range rects {
		x0 := toLng(float64(rc.ix0) * cell)
		x1 := toLng(float64(rc.ix1) * cell)
		y0 := box.minLat + float64(rc.iy0)*cell
		y1 := box.minLat + float64(rc.iy1)*cell
		r := Ring{
			{Lng: x0, Lat: y0}, {Lng: x1, Lat: y0},
			{Lng: x1, Lat: y1}, {Lng: x0, Lat: y1},
			{Lng: x0, Lat: y0},
		}
		if math.Abs(signedArea(r)) < 1e-15 {
			continue
		}
		g.Rings = append(g.Rings, r)
	}
	sort.Slice(g.Rings, func(i, j int) bool {
		return math.Abs(signedArea(g.Rings[i])) > math.Abs(signedArea(g.Rings[j]))
	})
	return g, nil
}

func geomBBox(g *Geom) bbox {
	b := ringBBox(g.Rings[0])
	for _, r := range g.Rings[1:] {
		rb := ringBBox(r)
		b.minLng = math.Min(b.minLng, rb.minLng)
		b.maxLng = math.Max(b.maxLng, rb.maxLng)
		b.minLat = math.Min(b.minLat, rb.minLat)
		b.maxLat = math.Max(b.maxLat, rb.maxLat)
	}
	return b
}

func pointSegDistUV(u, v float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = -((a[0]-u)*dx + (a[1]-v)*dy) / l2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	return math.Hypot(a[0]+t*dx-u, a[1]+t*dy-v)
}
