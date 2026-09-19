package app

import (
	"airspace/internal/geo"
)

// Conflict describes one spatial overlap involving an authorization
// polygon, with the locations where the overlap happens.
type Conflict struct {
	Type      string      `json:"type"` // route | mutual
	RequestID int64       `json:"request_id"`
	OtherID   int64       `json:"other_id"`
	OtherName string      `json:"other_name"`
	Points    []geo.Point `json:"points"`
}

// conflictsFor computes where polygon (belonging to requestID) clashes
// with active rescue corridors and with every other currently
// authorizing version. It is computed on read, so delayed or duplicated
// receipts can never leave a stale conflict view behind.
func (s *Server) conflictsFor(q dbtx, polygon geo.Polygon, requestID int64) ([]Conflict, error) {
	out := []Conflict{}

	routes, err := activeRoutes(q)
	if err != nil {
		return nil, err
	}
	for _, rt := range routes {
		if pts := geo.IntersectionPoints(polygon, rt.Corridor); len(pts) > 0 {
			out = append(out, Conflict{
				Type:      "route",
				RequestID: requestID,
				OtherID:   rt.ID,
				OtherName: rt.Name,
				Points:    pts,
			})
		}
	}

	actives, err := currentActiveVersions(q)
	if err != nil {
		return nil, err
	}
	for _, av := range actives {
		if av.Version.RequestID == requestID {
			continue
		}
		if pts := geo.IntersectionPoints(polygon, av.Version.Polygon); len(pts) > 0 {
			out = append(out, Conflict{
				Type:      "mutual",
				RequestID: requestID,
				OtherID:   av.Version.RequestID,
				OtherName: contractorName(q, av.ContractorID),
				Points:    pts,
			})
		}
	}
	return out, nil
}

// allConflicts is the command/ATC overview: every live clash in the
// airspace right now.
func (s *Server) allConflicts(q dbtx) ([]Conflict, error) {
	out := []Conflict{}
	actives, err := currentActiveVersions(q)
	if err != nil {
		return nil, err
	}
	routes, err := activeRoutes(q)
	if err != nil {
		return nil, err
	}
	for _, av := range actives {
		for _, rt := range routes {
			if pts := geo.IntersectionPoints(av.Version.Polygon, rt.Corridor); len(pts) > 0 {
				out = append(out, Conflict{
					Type:      "route",
					RequestID: av.Version.RequestID,
					OtherID:   rt.ID,
					OtherName: rt.Name,
					Points:    pts,
				})
			}
		}
	}
	for i := 0; i < len(actives); i++ {
		for j := i + 1; j < len(actives); j++ {
			a, b := actives[i], actives[j]
			if !windowsOverlap(a.Version.ValidFrom, a.Version.ValidTo, b.Version.ValidFrom, b.Version.ValidTo) {
				continue
			}
			if pts := geo.IntersectionPoints(a.Version.Polygon, b.Version.Polygon); len(pts) > 0 {
				out = append(out, Conflict{
					Type:      "mutual",
					RequestID: a.Version.RequestID,
					OtherID:   b.Version.RequestID,
					OtherName: contractorName(q, b.ContractorID),
					Points:    pts,
				})
			}
		}
	}
	return out, nil
}
