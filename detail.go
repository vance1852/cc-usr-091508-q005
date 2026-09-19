package main

import (
	"database/sql"
	"encoding/json"
)

// permitDetail builds the single source of truth for a permit: exactly one
// current/active version, full version history, spatial conflict locations,
// credential usage and who still has to acknowledge a freeze notice.
// Role-based projection happens here so handlers never leak cross-contractor
// data or credential tokens.
func (s *Store) permitDetail(permitID string, u *userRow) (map[string]any, error) {
	p, err := s.loadPermitCore(permitID)
	if err != nil {
		return nil, err
	}
	// Access control + tenant isolation.
	switch u.Role {
	case "atc":
		// full authorization handling
	case "rescue":
		// conflict overview only — token stripped below
	case "coordinator":
		if p.contractorID != u.ContractorID {
			return nil, fail(404, "permit not found")
		}
	case "pilot":
		if p.aircraftID != u.AircraftID {
			return nil, fail(404, "permit not found")
		}
	default:
		return nil, fail(403, "forbidden")
	}

	displayNames := s.displayNameMap()

	versions, err := s.versionViews(permitID, displayNames)
	if err != nil {
		return nil, err
	}
	var current any
	for _, vv := range versions {
		if vv.(versionView).Active {
			current = vv
		}
	}
	if current == nil && len(versions) > 0 {
		// DENIED/REVOKED permits have no active geometry version; the latest
		// version is still the unique effective statement of record.
		current = versions[len(versions)-1]
	}

	creds, err := s.credViews(permitID, u, displayNames)
	if err != nil {
		return nil, err
	}
	conflicts, err := s.conflictViews(permitID)
	if err != nil {
		return nil, err
	}
	pending, err := s.pendingAckViews(permitID, u)
	if err != nil {
		return nil, err
	}
	segs, err := s.segmentViews(permitID, u)
	if err != nil {
		return nil, err
	}

	var caps []string
	_ = json.Unmarshal([]byte(p.requiredCaps), &caps)

	out := map[string]any{
		"id":                      p.id,
		"missionName":             p.missionName,
		"state":                   p.state,
		"contractorId":            p.contractorID,
		"aircraftId":              p.aircraftID,
		"altitudeMin":             p.altMin,
		"altitudeMax":             p.altMax,
		"validFrom":               p.validFrom,
		"validUntil":              p.validUntil,
		"requiredCapabilities":    caps,
		"currentVersion":          current,
		"versions":                versions,
		"credentials":             creds,
		"conflicts":               conflicts,
		"pendingAcknowledgements": pending,
		"segments":                segs,
	}
	return out, nil
}

func (s *Store) displayNameMap() map[string]string {
	m := map[string]string{}
	rows, err := s.db.Query(`SELECT api_key, display_name FROM users`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var k, n string
		if rows.Scan(&k, &n) == nil {
			m[k] = n
		}
	}
	return m
}

func (s *Store) versionViews(permitID string, names map[string]string) ([]any, error) {
	rows, err := s.db.Query(
		`SELECT v.id, v.seq, v.kind, v.source, v.active, COALESCE(v.polygon,''),
		        v.alt_min, v.alt_max, v.valid_from, v.valid_until,
		        COALESCE(v.reason,''), COALESCE(v.decided_by,''), v.created_at
		 FROM permit_versions v WHERE v.permit_id=? ORDER BY v.seq`, permitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var v versionView
		var poly, by string
		var active int
		if err := rows.Scan(&v.ID, &v.Seq, &v.Kind, &v.Source, &active, &poly,
			&v.AltMin, &v.AltMax, &v.ValidFrom, &v.ValidUntil,
			&v.Reason, &by, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Active = active == 1
		if poly != "" {
			v.Polygon = json.RawMessage(poly)
		}
		if name, ok := names[by]; ok && by != "" {
			v.DecidedBy = name
		} else {
			v.DecidedBy = by
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) credViews(permitID string, u *userRow, names map[string]string) ([]credView, error) {
	ownerContractor := permitContractor(s, permitID)
	rows, err := s.db.Query(
		`SELECT c.id, c.version_id, pv.seq, c.aircraft_id, c.status,
		        COALESCE(c.freeze_reason,''), c.token, c.issued_at,
		        COALESCE(c.frozen_at,''), COALESCE(c.redeemed_at,''), COALESCE(c.redeemed_segment_id,'')
		 FROM credentials c JOIN permit_versions pv ON pv.id=c.version_id
		 WHERE c.permit_id=? ORDER BY c.issued_at`, permitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []credView{}
	for rows.Next() {
		var c credView
		var token string
		if err := rows.Scan(&c.ID, &c.VersionID, &c.VersionSeq, &c.AircraftID, &c.Status,
			&c.FreezeReason, &token, &c.IssuedAt,
			&c.FrozenAt, &c.RedeemedAt, &c.RedeemedSegmentID); err != nil {
			return nil, err
		}
		c.Frozen = c.Status == "FROZEN"
		c.SingleUse = true
		// Token visibility: ATC (full handling), the owning coordinator, and
		// the holder pilot. Rescue command never sees tokens.
		showToken := u.Role == "atc" ||
			(u.Role == "coordinator" && u.ContractorID == ownerContractor) ||
			(u.Role == "pilot" && c.AircraftID == u.AircraftID)
		if showToken {
			c.Token = token
		}
		out = append(out, c)
	}
	return out, nil
}

func permitContractor(s *Store, permitID string) string {
	var c string
	_ = s.db.QueryRow(`SELECT contractor_id FROM permits WHERE id=?`, permitID).Scan(&c)
	return c
}

func (s *Store) conflictViews(permitID string) ([]conflictView, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.kind, c.description, c.points, c.created_at, COALESCE(c.resolved_at,''),
		        COALESCE(c.permit_b,''), COALESCE(c.route_id,''), COALESCE(c.route_version,0)
		 FROM conflicts c WHERE c.permit_a=? OR c.permit_b=?
		 ORDER BY (c.resolved_at IS NOT NULL), c.created_at DESC`, permitID, permitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []conflictView{}
	for rows.Next() {
		var v conflictView
		var pts, other, route string
		var routeVer int
		if err := rows.Scan(&v.ID, &v.Kind, &v.Description, &pts, &v.CreatedAt, &v.ResolvedAt,
			&other, &route, &routeVer); err != nil {
			return nil, err
		}
		v.Open = v.ResolvedAt == ""
		_ = json.Unmarshal([]byte(pts), &v.Points)
		if route != "" {
			var name string
			_ = s.db.QueryRow(`SELECT name FROM rescue_routes WHERE id=?`, route).Scan(&name)
			v.Route = &routeRef{ID: route, Name: name, Version: routeVer}
		}
		if other != "" {
			var ac, mission string
			_ = s.db.QueryRow(`SELECT aircraft_id, mission_name FROM permits WHERE id=?`, other).
				Scan(&ac, &mission)
			v.OtherPermit = &otherPermitRef{ID: other, AircraftID: ac, MissionName: mission}
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) pendingAckViews(permitID string, u *userRow) ([]pendingAckView, error) {
	// A freeze notice is addressed to the holder pilot; "still pending" means
	// that pilot has not confirmed it yet. The holder is the pilot user whose
	// aircraft_id matches the notification target.
	rows, err := s.db.Query(
		`SELECT n.id, n.aircraft_id, n.type, n.title, n.created_at,
		        COALESCE(MAX(a.acked_at),''), COALESCE(hu.display_name,'')
		 FROM notifications n
		 LEFT JOIN users hu ON hu.aircraft_id=n.aircraft_id AND hu.role='pilot'
		 LEFT JOIN acknowledgements a ON a.notification_id=n.id AND a.api_key=hu.api_key
		 WHERE n.permit_id=?
		 GROUP BY n.id
		 ORDER BY n.created_at DESC`, permitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pendingAckView{}
	for rows.Next() {
		var v pendingAckView
		var ackedAt, ackedBy string
		if err := rows.Scan(&v.NotificationID, &v.AircraftID, &v.Type, &v.Title,
			&v.CreatedAt, &ackedAt, &ackedBy); err != nil {
			return nil, err
		}
		if u.Role == "pilot" && v.AircraftID != u.AircraftID {
			continue
		}
		v.Acknowledged = ackedAt != ""
		v.AcknowledgedBy = ackedBy
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) segmentViews(permitID string, u *userRow) ([]segmentView, error) {
	rows, err := s.db.Query(
		`SELECT id, credential_id, aircraft_id, started_at, COALESCE(ended_at,''),
		        COALESCE(track,''), deviation, COALESCE(deviation_detail,''),
		        COALESCE(disposition,''), recorded_at
		 FROM segments WHERE permit_id=? ORDER BY started_at`, permitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []segmentView{}
	for rows.Next() {
		var v segmentView
		var track string
		var ended, detail, disp sql.NullString
		var dev int
		if err := rows.Scan(&v.ID, &v.CredentialID, &v.AircraftID, &v.StartedAt, &ended,
			&track, &dev, &detail, &disp, &v.RecordedAt); err != nil {
			return nil, err
		}
		if u.Role == "pilot" && v.AircraftID != u.AircraftID {
			continue
		}
		v.EndedAt = nullStr(ended)
		v.Deviation = dev == 1
		v.DeviationDetail = nullStr(detail)
		v.Disposition = nullStr(disp)
		v.Immutable = ended.Valid
		if track != "" && track != "null" {
			_ = json.Unmarshal([]byte(track), &v.Track)
		}
		out = append(out, v)
	}
	return out, nil
}

// ---------- lists & dashboards ----------

type permitSummary struct {
	ID             string          `json:"id"`
	MissionName    string          `json:"missionName"`
	State          string          `json:"state"`
	ContractorID   string          `json:"contractorId"`
	AircraftID     string          `json:"aircraftId"`
	ValidFrom      string          `json:"validFrom"`
	ValidUntil     string          `json:"validUntil"`
	CurrentPolygon json.RawMessage `json:"currentPolygon,omitempty"`
	OpenConflicts  int             `json:"openConflicts"`
	FrozenCreds    int             `json:"frozenCredentials"`
	ActiveCreds    int             `json:"activeCredentials"`
}

func (s *Store) listPermits(u *userRow) ([]permitSummary, error) {
	q := `SELECT p.id, p.mission_name, p.state, p.contractor_id, p.aircraft_id,
	                p.valid_from, p.valid_until, COALESCE(pv.polygon,''), pv.active
	       FROM permits p JOIN permit_versions pv ON pv.id=p.current_version_id`
	var args []interface{}
	switch u.Role {
	case "coordinator", "pilot":
		if u.Role == "coordinator" {
			q += ` WHERE p.contractor_id=?`
			args = append(args, u.ContractorID)
		} else {
			q += ` WHERE p.aircraft_id=?`
			args = append(args, u.AircraftID)
		}
	case "atc", "rescue":
		// full register
	default:
		return nil, fail(403, "forbidden")
	}
	q += ` ORDER BY p.created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []permitSummary{}
	for rows.Next() {
		var ps permitSummary
		var poly string
		var pvActive int
		if err := rows.Scan(&ps.ID, &ps.MissionName, &ps.State, &ps.ContractorID,
			&ps.AircraftID, &ps.ValidFrom, &ps.ValidUntil, &poly, &pvActive); err != nil {
			return nil, err
		}
		if pvActive == 1 {
			ps.CurrentPolygon = json.RawMessage(poly)
		}
		_ = s.db.QueryRow(
			`SELECT COUNT(*) FROM conflicts
			 WHERE (permit_a=? OR permit_b=?) AND resolved_at IS NULL`,
			ps.ID, ps.ID).Scan(&ps.OpenConflicts)
		_ = s.db.QueryRow(
			`SELECT COUNT(*) FROM credentials WHERE permit_id=? AND status='FROZEN'`,
			ps.ID).Scan(&ps.FrozenCreds)
		_ = s.db.QueryRow(
			`SELECT COUNT(*) FROM credentials WHERE permit_id=? AND status='ISSUED'`,
			ps.ID).Scan(&ps.ActiveCreds)
		out = append(out, ps)
	}
	return out, nil
}

// rescueDashboard is the conflict overview for rescue command: open conflicts
// with locations, affected aircraft, frozen counts, unacked notices.
func (s *Store) rescueDashboard() map[string]any {
	type row struct {
		id, kind, desc, points, createdAt    string
		permitA, permitB, missionA, missionB string
		aircraftA, aircraftB                 string
		routeID, routeName                   string
		routeVersion                         int
	}
	rows, err := s.db.Query(
		`SELECT c.id, c.kind, c.description, c.points, c.created_at,
		        c.permit_a, COALESCE(c.permit_b,''),
		        pa.mission_name, COALESCE(pb.mission_name,''),
		        pa.aircraft_id, COALESCE(pb.aircraft_id,''),
		        COALESCE(c.route_id,''), COALESCE(r.name,''), COALESCE(c.route_version,0)
		 FROM conflicts c
		 JOIN permits pa ON pa.id=c.permit_a
		 LEFT JOIN permits pb ON pb.id=c.permit_b
		 LEFT JOIN rescue_routes r ON r.id=c.route_id
		 WHERE c.resolved_at IS NULL
		 ORDER BY c.created_at DESC`)
	if err != nil {
		return map[string]any{"openConflicts": []any{}}
	}
	defer rows.Close()
	type openConflict struct {
		ID          string    `json:"id"`
		Kind        string    `json:"kind"`
		Description string    `json:"description"`
		Points      []Point   `json:"points"`
		CreatedAt   string    `json:"createdAt"`
		PermitA     string    `json:"permitA"`
		MissionA    string    `json:"missionA"`
		AircraftA   string    `json:"aircraftA"`
		PermitB     string    `json:"permitB,omitempty"`
		MissionB    string    `json:"missionB,omitempty"`
		AircraftB   string    `json:"aircraftB,omitempty"`
		Route       *routeRef `json:"route,omitempty"`
	}
	opens := []openConflict{}
	affected := map[string]bool{}
	for rows.Next() {
		var r2 struct {
			id, kind, desc, points, createdAt        string
			permitA, permitB, missionA, missionB     string
			aircraftA, aircraftB, routeID, routeName string
			routeVersion                             int
		}
		if err := rows.Scan(&r2.id, &r2.kind, &r2.desc, &r2.points, &r2.createdAt,
			&r2.permitA, &r2.permitB, &r2.missionA, &r2.missionB,
			&r2.aircraftA, &r2.aircraftB, &r2.routeID, &r2.routeName,
			&r2.routeVersion); err != nil {
			continue
		}
		oc := openConflict{
			ID: r2.id, Kind: r2.kind, Description: r2.desc, CreatedAt: r2.createdAt,
			PermitA: r2.permitA, MissionA: r2.missionA, AircraftA: r2.aircraftA,
			PermitB: r2.permitB, MissionB: r2.missionB, AircraftB: r2.aircraftB,
		}
		_ = json.Unmarshal([]byte(r2.points), &oc.Points)
		if r2.routeID != "" {
			oc.Route = &routeRef{ID: r2.routeID, Name: r2.routeName, Version: r2.routeVersion}
		}
		opens = append(opens, oc)
		affected[r2.aircraftA] = true
		if r2.aircraftB != "" {
			affected[r2.aircraftB] = true
		}
	}

	var frozen, issued, redeemed int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE status='FROZEN'`).Scan(&frozen)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE status='ISSUED'`).Scan(&issued)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE status='REDEEMED'`).Scan(&redeemed)

	var pendingAcks int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM notifications n
		 WHERE NOT EXISTS (SELECT 1 FROM acknowledgements a WHERE a.notification_id=n.id)`).
		Scan(&pendingAcks)

	// Who still has to respond.
	type pending struct {
		NotificationID string `json:"notificationId"`
		AircraftID     string `json:"aircraftId"`
		Type           string `json:"type"`
		Title          string `json:"title"`
		CreatedAt      string `json:"createdAt"`
	}
	pendList := []pending{}
	pr, err := s.db.Query(
		`SELECT n.id, n.aircraft_id, n.type, n.title, n.created_at
		 FROM notifications n
		 WHERE NOT EXISTS (SELECT 1 FROM acknowledgements a WHERE a.notification_id=n.id)
		 ORDER BY n.created_at DESC`)
	if err == nil {
		defer pr.Close()
		for pr.Next() {
			var p pending
			if pr.Scan(&p.NotificationID, &p.AircraftID, &p.Type, &p.Title, &p.CreatedAt) == nil {
				pendList = append(pendList, p)
			}
		}
	}

	return map[string]any{
		"openConflicts":           opens,
		"affectedAircraftCount":   len(affected),
		"frozenCredentials":       frozen,
		"issuedCredentials":       issued,
		"redeemedCredentials":     redeemed,
		"pendingAcknowledgements": pendList,
	}
}
