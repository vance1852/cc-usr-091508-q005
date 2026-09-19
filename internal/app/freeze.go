package app

import (
	"database/sql"
	"fmt"
	"time"

	"airspace/internal/geo"
	"airspace/internal/store"
)

// freezeCandidate is an active credential joined with everything the
// freeze rules need to look at.
type freezeCandidate struct {
	Cred       store.Credential
	Polygon    geo.Polygon
	ValidFrom  time.Time
	ValidTo    time.Time
	LinkStatus string
	Callsign   string
}

func loadFreezeCandidates(q dbtx) ([]freezeCandidate, error) {
	rows, err := q.Query(`SELECT c.id, c.version_id, c.request_id, c.aircraft_id, c.pilot_id, c.token,
		c.status, c.issued_at, c.used_at, c.frozen_at, c.frozen_reason,
		v.polygon, v.valid_from, v.valid_to, a.link_status, a.callsign
		FROM credentials c
		JOIN auth_versions v ON v.id = c.version_id
		JOIN aircraft a ON a.id = c.aircraft_id
		WHERE c.status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []freezeCandidate
	for rows.Next() {
		var fc freezeCandidate
		var poly, vf, vt string
		var issued string
		var used, frozen sql.NullString
		if err := rows.Scan(&fc.Cred.ID, &fc.Cred.VersionID, &fc.Cred.RequestID, &fc.Cred.AircraftID,
			&fc.Cred.PilotID, &fc.Cred.Token, &fc.Cred.Status, &issued, &used, &frozen,
			&fc.Cred.FrozenReason, &poly, &vf, &vt, &fc.LinkStatus, &fc.Callsign); err != nil {
			return nil, err
		}
		fc.Polygon = store.UnmarshalPolygon(poly)
		fc.ValidFrom, fc.ValidTo = store.ParseTime(vf), store.ParseTime(vt)
		fc.Cred.IssuedAt = store.ParseTime(issued)
		fc.Cred.UsedAt = scanNullTime(used)
		fc.Cred.FrozenAt = scanNullTime(frozen)
		out = append(out, fc)
	}
	return out, rows.Err()
}

// freezeCredential flips an active credential to frozen and leaves the
// holder an acknowledgable notification, all inside the same transaction.
func freezeCredential(q dbtx, c store.Credential, reason, message string, now time.Time) error {
	res, err := q.Exec(`UPDATE credentials SET status='frozen', frozen_at=?, frozen_reason=?
		WHERE id = ? AND status = 'active'`, store.FmtTime(now), reason, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already left the active state; nothing to do
	}
	credID := c.ID
	return notify(q, c.PilotID, &c.RequestID, &credID, store.NotifFrozen, message, now)
}

// evaluateFreezes applies every freeze rule. It runs inside the same
// transaction as the state change that triggered it (decision, route
// update, link loss), so a credential can never be observed active
// between the trigger and the freeze. It only ever freezes; unfreezing
// is an explicit human action by ATC.
func (s *Server) evaluateFreezes(q dbtx, now time.Time) error {
	routes, err := activeRoutes(q)
	if err != nil {
		return err
	}
	cands, err := loadFreezeCandidates(q)
	if err != nil {
		return err
	}

	// R1: aircraft link lost -> freeze its active credentials.
	// R2: authorized polygon intersects an active rescue corridor.
	for _, fc := range cands {
		if fc.LinkStatus == "lost" {
			if err := freezeCredential(q, fc.Cred, store.FreezeLinkLost,
				fmt.Sprintf("飞行器 %s 失联，凭证已冻结；确认后等待空管指令", fc.Callsign), now); err != nil {
				return err
			}
			continue
		}
		for _, rt := range routes {
			if geo.Intersects(fc.Polygon, rt.Corridor) {
				if err := freezeCredential(q, fc.Cred, store.FreezeRouteConflict,
					fmt.Sprintf("授权范围与救援航线「%s」走廊相交，凭证已冻结；确认后等待空管指令", rt.Name), now); err != nil {
					return err
				}
				break
			}
		}
	}

	// R3: two currently-authorizing versions with overlapping time windows
	// intersect each other -> the more recently issued credential freezes
	// (first approval keeps priority).
	if err := s.evaluateMutual(q, now); err != nil {
		return err
	}
	return nil
}

func (s *Server) evaluateMutual(q dbtx, now time.Time) error {
	// Reload: R1/R2 may already have frozen some credentials.
	cands, err := loadFreezeCandidates(q)
	if err != nil {
		return err
	}
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			a, b := cands[i], cands[j]
			if a.Cred.RequestID == b.Cred.RequestID {
				continue
			}
			if !windowsOverlap(a.ValidFrom, a.ValidTo, b.ValidFrom, b.ValidTo) {
				continue
			}
			if !geo.Intersects(a.Polygon, b.Polygon) {
				continue
			}
			// Freeze the later-issued credential; earlier approval wins.
			loser, winner := a, b
			if b.Cred.IssuedAt.After(a.Cred.IssuedAt) {
				loser, winner = b, a
			}
			msg := fmt.Sprintf("授权范围与请求 #%d（%s）的在飞范围相交，后签发凭证已冻结；确认后等待空管指令",
				winner.Cred.RequestID, winner.Callsign)
			if err := freezeCredential(q, loser.Cred, store.FreezeMutual, msg, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func windowsOverlap(aFrom, aTo, bFrom, bTo time.Time) bool {
	return aFrom.Before(bTo) && bFrom.Before(aTo)
}
