package geo

import "testing"

func square(lat, lng, size float64) Polygon {
	return Polygon{Vertices: []Point{
		{Lat: lat, Lng: lng},
		{Lat: lat, Lng: lng + size},
		{Lat: lat + size, Lng: lng + size},
		{Lat: lat + size, Lng: lng},
	}}
}

func TestIntersectsOverlapping(t *testing.T) {
	a := square(31.0, 121.0, 0.01)
	b := square(31.005, 121.005, 0.01)
	if !Intersects(a, b) || !Intersects(b, a) {
		t.Fatal("overlapping squares must intersect")
	}
	pts := IntersectionPoints(a, b)
	if len(pts) < 2 {
		t.Fatalf("expected intersection points, got %v", pts)
	}
}

func TestIntersectsDisjoint(t *testing.T) {
	a := square(31.0, 121.0, 0.01)
	b := square(32.0, 122.0, 0.01)
	if Intersects(a, b) {
		t.Fatal("disjoint squares must not intersect")
	}
	if pts := IntersectionPoints(a, b); len(pts) != 0 {
		t.Fatalf("expected no points, got %v", pts)
	}
}

func TestIntersectsContained(t *testing.T) {
	outer := square(31.0, 121.0, 0.02)
	inner := square(31.005, 121.005, 0.005)
	if !Intersects(outer, inner) {
		t.Fatal("contained polygon intersects container")
	}
	pts := IntersectionPoints(outer, inner)
	if len(pts) != 4 { // all four inner vertices lie inside outer
		t.Fatalf("expected 4 contained vertices, got %d: %v", len(pts), pts)
	}
}

func TestContains(t *testing.T) {
	outer := square(31.0, 121.0, 0.02)
	inner := square(31.005, 121.005, 0.005)
	if !Contains(outer, inner) {
		t.Fatal("inner square must be contained")
	}
	if Contains(inner, outer) {
		t.Fatal("outer must not be contained in inner")
	}
	stickingOut := square(31.015, 121.015, 0.01) // partially outside
	if Contains(outer, stickingOut) {
		t.Fatal("partially outside polygon must not be contained")
	}
}

func TestTouchingBoundariesIntersect(t *testing.T) {
	a := square(31.0, 121.0, 0.01)
	b := square(31.0, 121.01, 0.01) // shares edge lng=121.01
	if !Intersects(a, b) {
		t.Fatal("touching boundaries count as intersecting")
	}
}

func TestPointInPolygon(t *testing.T) {
	p := square(31.0, 121.0, 0.01)
	if !PointInPolygon(Point{Lat: 31.005, Lng: 121.005}, p) {
		t.Fatal("center must be inside")
	}
	if PointInPolygon(Point{Lat: 31.02, Lng: 121.02}, p) {
		t.Fatal("far point must be outside")
	}
	if !PointInPolygon(Point{Lat: 31.0, Lng: 121.005}, p) {
		t.Fatal("boundary point counts as inside")
	}
}

func TestArea(t *testing.T) {
	p := square(31.0, 121.0, 0.01)
	if got := p.Area(); got < 9.9e-5 || got > 1.1e-4 {
		t.Fatalf("unexpected area %v", got)
	}
}
