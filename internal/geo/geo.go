// Package geo implements the small amount of planar geometry the
// authorization service needs: polygon intersection, containment and
// intersection-point calculation. Coordinates are WGS84 lat/lng treated
// as planar, which is adequate for the small areas of an emergency scene.
package geo

import "math"

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Polygon is a single closed ring. The ring is implicitly closed: the
// last vertex connects back to the first.
type Polygon struct {
	Vertices []Point `json:"vertices"`
}

const eps = 1e-9

// Valid reports whether the polygon can enclose an area.
func (p Polygon) Valid() bool { return len(p.Vertices) >= 3 }

func (p Polygon) edges() [][2]Point {
	n := len(p.Vertices)
	es := make([][2]Point, 0, n)
	for i := 0; i < n; i++ {
		es = append(es, [2]Point{p.Vertices[i], p.Vertices[(i+1)%n]})
	}
	return es
}

// Area returns the absolute planar area (shoelace formula).
func (p Polygon) Area() float64 {
	var s float64
	n := len(p.Vertices)
	for i := 0; i < n; i++ {
		a, b := p.Vertices[i], p.Vertices[(i+1)%n]
		s += a.Lng*b.Lat - b.Lng*a.Lat
	}
	return math.Abs(s) / 2
}

// cross is the 2D cross product of vectors oa and ob (lng=x, lat=y).
func cross(o, a, b Point) float64 {
	return (a.Lng-o.Lng)*(b.Lat-o.Lat) - (a.Lat-o.Lat)*(b.Lng-o.Lng)
}

func onSegment(a, b, p Point) bool {
	if math.Abs(cross(a, b, p)) > eps {
		return false
	}
	return p.Lng >= math.Min(a.Lng, b.Lng)-eps && p.Lng <= math.Max(a.Lng, b.Lng)+eps &&
		p.Lat >= math.Min(a.Lat, b.Lat)-eps && p.Lat <= math.Max(a.Lat, b.Lat)+eps
}

// segmentsIntersect reports whether segments ab and cd touch or cross.
func segmentsIntersect(a, b, c, d Point) bool {
	d1 := cross(c, d, a)
	d2 := cross(c, d, b)
	d3 := cross(a, b, c)
	d4 := cross(a, b, d)
	if ((d1 > eps && d2 < -eps) || (d1 < -eps && d2 > eps)) &&
		((d3 > eps && d4 < -eps) || (d3 < -eps && d4 > eps)) {
		return true
	}
	if math.Abs(d1) <= eps && onSegment(c, d, a) {
		return true
	}
	if math.Abs(d2) <= eps && onSegment(c, d, b) {
		return true
	}
	if math.Abs(d3) <= eps && onSegment(a, b, c) {
		return true
	}
	if math.Abs(d4) <= eps && onSegment(a, b, d) {
		return true
	}
	return false
}

// segmentsProperlyCross reports whether ab and cd cross in their
// interiors (mere endpoint touching does not count).
func segmentsProperlyCross(a, b, c, d Point) bool {
	d1 := cross(c, d, a)
	d2 := cross(c, d, b)
	d3 := cross(a, b, c)
	d4 := cross(a, b, d)
	return ((d1 > eps && d2 < -eps) || (d1 < -eps && d2 > eps)) &&
		((d3 > eps && d4 < -eps) || (d3 < -eps && d4 > eps))
}

// segIntersectionPoint returns the intersection point of segments ab and
// cd, if they intersect (including touching). For overlapping collinear
// segments it returns one of the shared endpoints.
func segIntersectionPoint(a, b, c, d Point) (Point, bool) {
	if !segmentsIntersect(a, b, c, d) {
		return Point{}, false
	}
	den := (b.Lng-a.Lng)*(d.Lat-c.Lat) - (b.Lat-a.Lat)*(d.Lng-c.Lng)
	if math.Abs(den) < eps {
		// Collinear or parallel: return a shared endpoint if any.
		for _, p := range []Point{a, b} {
			if onSegment(c, d, p) {
				return p, true
			}
		}
		for _, p := range []Point{c, d} {
			if onSegment(a, b, p) {
				return p, true
			}
		}
		return Point{}, false
	}
	t := ((c.Lng-a.Lng)*(d.Lat-c.Lat) - (c.Lat-a.Lat)*(d.Lng-c.Lng)) / den
	return Point{
		Lat: a.Lat + t*(b.Lat-a.Lat),
		Lng: a.Lng + t*(b.Lng-a.Lng),
	}, true
}

// PointInPolygon reports whether pt lies inside poly. Points exactly on
// the boundary count as inside.
func PointInPolygon(pt Point, poly Polygon) bool {
	n := len(poly.Vertices)
	if n < 3 {
		return false
	}
	inside := false
	for i := 0; i < n; i++ {
		a := poly.Vertices[i]
		b := poly.Vertices[(i+1)%n]
		if onSegment(a, b, pt) {
			return true
		}
		// Ray cast towards +lng.
		if (a.Lat > pt.Lat) != (b.Lat > pt.Lat) {
			x := a.Lng + (pt.Lat-a.Lat)*(b.Lng-a.Lng)/(b.Lat-a.Lat)
			if x > pt.Lng {
				inside = !inside
			}
		}
	}
	return inside
}

// Intersects reports whether polygons a and b have any area or boundary
// in common.
func Intersects(a, b Polygon) bool {
	if !a.Valid() || !b.Valid() {
		return false
	}
	for _, ea := range a.edges() {
		for _, eb := range b.edges() {
			if segmentsIntersect(ea[0], ea[1], eb[0], eb[1]) {
				return true
			}
		}
	}
	// One polygon fully inside the other.
	if PointInPolygon(a.Vertices[0], b) || PointInPolygon(b.Vertices[0], a) {
		return true
	}
	return false
}

// IntersectionPoints returns the locations where a and b meet: edge
// crossing points plus vertices of either polygon lying inside the other.
// The result is deduplicated. It is empty when the polygons are disjoint.
func IntersectionPoints(a, b Polygon) []Point {
	var pts []Point
	add := func(p Point) {
		for _, q := range pts {
			if math.Abs(p.Lat-q.Lat) < 1e-7 && math.Abs(p.Lng-q.Lng) < 1e-7 {
				return
			}
		}
		pts = append(pts, p)
	}
	for _, ea := range a.edges() {
		for _, eb := range b.edges() {
			if p, ok := segIntersectionPoint(ea[0], ea[1], eb[0], eb[1]); ok {
				add(p)
			}
		}
	}
	for _, v := range a.Vertices {
		if PointInPolygon(v, b) {
			add(v)
		}
	}
	for _, v := range b.Vertices {
		if PointInPolygon(v, a) {
			add(v)
		}
	}
	return pts
}

// Contains reports whether inner lies entirely within outer (boundary
// contact allowed). Used to validate that a shrunk authorization stays
// inside the previously approved area.
func Contains(outer, inner Polygon) bool {
	if !outer.Valid() || !inner.Valid() {
		return false
	}
	for _, v := range inner.Vertices {
		if !PointInPolygon(v, outer) {
			return false
		}
	}
	for _, ei := range inner.edges() {
		for _, eo := range outer.edges() {
			if segmentsProperlyCross(ei[0], ei[1], eo[0], eo[1]) {
				return false
			}
		}
	}
	return true
}
