package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ---------- wire views ----------

type routeRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type otherPermitRef struct {
	ID          string `json:"id"`
	AircraftID  string `json:"aircraftId"`
	MissionName string `json:"missionName"`
}

type conflictView struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Open        bool            `json:"open"`
	Description string          `json:"description"`
	Points      []Point         `json:"points"`
	CreatedAt   string          `json:"createdAt"`
	ResolvedAt  string          `json:"resolvedAt,omitempty"`
	Route       *routeRef       `json:"route,omitempty"`
	OtherPermit *otherPermitRef `json:"otherPermit,omitempty"`
}

type versionView struct {
	ID         string          `json:"id"`
	Seq        int             `json:"seq"`
	Kind       string          `json:"kind"`
	Source     string          `json:"source"`
	Active     bool            `json:"active"`
	Polygon    json.RawMessage `json:"polygon"`
	AltMin     float64         `json:"altitudeMin"`
	AltMax     float64         `json:"altitudeMax"`
	ValidFrom  string          `json:"validFrom"`
	ValidUntil string          `json:"validUntil"`
	Reason     string          `json:"reason,omitempty"`
	DecidedBy  string          `json:"decidedBy,omitempty"`
	CreatedAt  string          `json:"createdAt"`
}

type credView struct {
	ID                string `json:"id"`
	VersionID         string `json:"versionId"`
	VersionSeq        int    `json:"versionSeq"`
	AircraftID        string `json:"aircraftId"`
	Status            string `json:"status"`
	Frozen            bool   `json:"frozen"`
	FreezeReason      string `json:"freezeReason,omitempty"`
	SingleUse         bool   `json:"singleUse"`
	Token             string `json:"token,omitempty"`
	IssuedAt          string `json:"issuedAt"`
	FrozenAt          string `json:"frozenAt,omitempty"`
	RedeemedAt        string `json:"redeemedAt,omitempty"`
	RedeemedSegmentID string `json:"redeemedSegmentId,omitempty"`
}

type pendingAckView struct {
	NotificationID string `json:"notificationId"`
	AircraftID     string `json:"aircraftId"`
	Type           string `json:"type"`
	Title          string `json:"title"`
	CreatedAt      string `json:"createdAt"`
	Acknowledged   bool   `json:"acknowledged"`
	AcknowledgedBy string `json:"acknowledgedBy,omitempty"`
}

type notificationView struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	PermitID       string `json:"permitId"`
	CredentialID   string `json:"credentialId,omitempty"`
	CreatedAt      string `json:"createdAt"`
	AcknowledgedAt string `json:"acknowledgedAt,omitempty"`
}

type segmentView struct {
	ID              string      `json:"id"`
	CredentialID    string      `json:"credentialId"`
	AircraftID      string      `json:"aircraftId"`
	StartedAt       string      `json:"startedAt"`
	EndedAt         string      `json:"endedAt,omitempty"`
	Track           [][]float64 `json:"track"`
	Deviation       bool        `json:"deviation"`
	DeviationDetail string      `json:"deviationDetail,omitempty"`
	Disposition     string      `json:"disposition,omitempty"`
	RecordedAt      string      `json:"recordedAt"`
	Immutable       bool        `json:"immutable"`
}

func nullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// ---------- rescue-route management ----------

func (s *Store) upsertRoute(u *userRow, routeID string, dto routeDTO) (*routeRef, error) {
	if u.Role != "atc" && u.Role != "rescue" {
		return nil, fail(403, "only ATC or rescue command can maintain rescue routes")
	}
	g, err := ParseGeom(dto.Corridor)
	if err != nil {
		return nil, fail(400, err.Error())
	}
	if dto.Name == "" {
		return nil, fail(400, "name is required")
	}
	now := s.now().Format(time.RFC3339)

	if routeID == "" {
		routeID = newID("rt")
		err = s.withTx(func(tx *sql.Tx) error {
			_, e := tx.Exec(
				`INSERT INTO rescue_routes(id, name, version, corridor, created_by, created_at, active)
				 VALUES(?,?,1,?,?,?,1)`,
				routeID, dto.Name, string(g.Marshal()), u.APIKey, now)
			return e
		})
	} else {
		err = s.withTx(func(tx *sql.Tx) error {
			var old string
			err := tx.QueryRow(`SELECT corridor FROM rescue_routes WHERE id=? AND active=1`, routeID).Scan(&old)
			if err == sql.ErrNoRows {
				return fail(404, "route not found")
			}
			if err != nil {
				return err
			}
			// A corridor update is published as a new route version. Close all
			// open conflicts against the previous version; evaluateConflicts()
			// then opens version-matched conflicts and freezes intersecting
			// permits' credentials against the new corridor.
			if _, err := tx.Exec(
				`UPDATE conflicts SET resolved_at=? WHERE route_id=? AND resolved_at IS NULL`,
				now, routeID); err != nil {
				return err
			}
			_, err = tx.Exec(
				`UPDATE rescue_routes SET name=?, version=version+1, corridor=? WHERE id=?`,
				dto.Name, string(g.Marshal()), routeID)
			if err != nil {
				return err
			}
			var ver int
			_ = tx.QueryRow(`SELECT version FROM rescue_routes WHERE id=?`, routeID).Scan(&ver)
			s.event(tx, "", "ROUTE_UPDATED", "ROUTE_VERSION", u.APIKey,
				fmt.Sprintf("%s v%d", routeID, ver))
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	if err := s.withTx(func(tx *sql.Tx) error { return s.evaluateConflicts(tx) }); err != nil {
		return nil, err
	}
	var name string
	var ver int
	_ = s.db.QueryRow(`SELECT name, version FROM rescue_routes WHERE id=?`, routeID).Scan(&name, &ver)
	return &routeRef{ID: routeID, Name: name, Version: ver}, nil
}

// ---------- credential redemption (single use) ----------

type redeemResult struct {
	CredentialID string `json:"credentialId"`
	SegmentID    string `json:"segmentId"`
	Status       string `json:"status"`
	Message      string `json:"message"`
}

func (s *Store) redeem(u *userRow, token, note string) (*redeemResult, error) {
	if u.Role != "pilot" {
		return nil, fail(403, "only the assigned pilot can redeem a credential")
	}
	var c credentialRow
	var versionID string
	err := s.db.QueryRow(
		`SELECT id, permit_id, version_id, aircraft_id, status, COALESCE(freeze_reason,'')
		 FROM credentials WHERE token=?`, token).
		Scan(&c.ID, &c.PermitID, &versionID, &c.AircraftID, &c.Status, &c.FreezeReason)
	if err == sql.ErrNoRows {
		return nil, fail(404, "invalid credential token")
	}
	if err != nil {
		return nil, err
	}
	if c.AircraftID != u.AircraftID {
		return nil, fail(403, "credential was issued to a different aircraft")
	}
	ac, _ := s.aircraft(c.AircraftID)
	if ac != nil && ac.LinkStatus == "lost" {
		return nil, fail(403, "aircraft link is lost; credential is held")
	}
	switch c.Status {
	case "FROZEN":
		return nil, fail(423, "credential is frozen: "+c.FreezeReason)
	case "REDEEMED":
		return nil, fail(409, "credential is single-use and has already been redeemed")
	case "EXPIRED":
		return nil, fail(410, "credential has expired")
	}
	// Time window check.
	p, err := s.loadPermitCore(c.PermitID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Before(p.from) || now.After(p.until) {
		return nil, fail(410, "outside the authorized time window")
	}

	segID := newID("seg")
	err = s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE credentials SET status='REDEEMED', redeemed_at=?, redeemed_segment_id=?
			 WHERE id=? AND status='ISSUED'`,
			now.Format(time.RFC3339), segID, c.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fail(409, "credential was already used")
		}
		_, err = tx.Exec(`INSERT INTO segments
		 (id, credential_id, aircraft_id, permit_id, started_at, recorded_at, recorded_by)
		 VALUES(?,?,?,?,?,?,?)`,
			segID, c.ID, c.AircraftID, c.PermitID,
			now.Format(time.RFC3339), now.Format(time.RFC3339), u.APIKey)
		if err != nil {
			return err
		}
		s.event(tx, c.PermitID, "CREDENTIAL_REDEEMED", "PILOT", u.APIKey, c.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &redeemResult{
		CredentialID: c.ID, SegmentID: segID, Status: "REDEEMED",
		Message: "凭证仅可使用一次，已兑换并建立航段；起飞后请按实际航迹报备。",
	}, nil
}

// ---------- segment reporting: deviations are recorded truthfully, never rewritten ----------

func (s *Store) reportSegment(u *userRow, segID string, dto segmentDTO) (*segmentView, error) {
	if u.Role != "pilot" {
		return nil, fail(403, "only the pilot can report segment tracks")
	}
	var seg struct {
		id, cred, aircraft, permit, started, ended string
	}
	var ended sql.NullString
	err := s.db.QueryRow(
		`SELECT id, credential_id, aircraft_id, permit_id, started_at, ended_at
		 FROM segments WHERE id=?`, segID).
		Scan(&seg.id, &seg.cred, &seg.aircraft, &seg.permit, &seg.started, &ended)
	if err == sql.ErrNoRows {
		return nil, fail(404, "segment not found")
	}
	if err != nil {
		return nil, err
	}
	if seg.aircraft != u.AircraftID {
		return nil, fail(403, "not your segment")
	}
	if ended.Valid {
		// 已经起飞的航段如实登记偏航处置，不能回写成未执行 / 不得覆盖。
		return nil, fail(409, "segment already closed: flight records are append-only and cannot be rewritten")
	}
	end := dto.EndedAt
	if end == "" {
		end = s.now().Format(time.RFC3339)
	}
	if _, perr := time.Parse(time.RFC3339, end); perr != nil {
		return nil, fail(400, "endedAt must be RFC3339")
	}

	// Server-side truth: deviation is computed against the exact authorized
	// version polygon, never taken from the client payload.
	var poly string
	err = s.db.QueryRow(
		`SELECT pv.polygon FROM credentials c
		 JOIN permit_versions pv ON pv.id=c.version_id WHERE c.id=?`, seg.cred).Scan(&poly)
	if err != nil {
		return nil, err
	}
	g := mustParse(poly)
	outPts, maxDist := outsideTrack(g, dto.Track)
	deviation := len(outPts) > 0
	detail := ""
	if deviation {
		detail = fmt.Sprintf("航迹有 %d 个采样点偏离授权边界，最远约 %.0f m；首点 [%.6f,%.6f]",
			len(outPts), maxDist, outPts[0].Lng, outPts[0].Lat)
	}
	track, _ := json.Marshal(dto.Track)
	recAt := s.now().Format(time.RFC3339)

	err = s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE segments SET ended_at=?, track=?, deviation=?, deviation_detail=?, disposition=?, recorded_at=?
			 WHERE id=? AND ended_at IS NULL`,
			end, string(track), deviation, detail, dto.Disposition, recAt, segID)
		if err != nil {
			return err
		}
		s.event(tx, seg.permit, "SEGMENT_CLOSED", "PILOT", u.APIKey,
			fmt.Sprintf("%s deviation=%v detail=%s", segID, deviation, detail))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.segmentByID(segID)
}

func (s *Store) segmentByID(segID string) (*segmentView, error) {
	var v segmentView
	var track sql.NullString
	var ended sql.NullString
	var devDetail, disp sql.NullString
	var dev int
	err := s.db.QueryRow(
		`SELECT id, credential_id, aircraft_id, started_at, ended_at, track,
		        deviation, deviation_detail, disposition, recorded_at
		 FROM segments WHERE id=?`, segID).
		Scan(&v.ID, &v.CredentialID, &v.AircraftID, &v.StartedAt, &ended, &track,
			&dev, &devDetail, &disp, &v.RecordedAt)
	if err != nil {
		return nil, err
	}
	v.EndedAt = nullStr(ended)
	v.Deviation = dev == 1
	v.DeviationDetail = nullStr(devDetail)
	v.Disposition = nullStr(disp)
	v.Immutable = ended.Valid
	if track.Valid && track.String != "" && track.String != "null" {
		_ = json.Unmarshal([]byte(track.String), &v.Track)
	}
	return &v, nil
}

// outsideTrack returns track points outside the authorized geometry plus the
// approximate maximum excursion in meters (equirectangular, local scene).
func outsideTrack(g *Geom, track [][]float64) ([]Point, float64) {
	out := []Point{}
	maxD := 0.0
	for _, tp := range track {
		if len(tp) < 2 {
			continue
		}
		p := Point{Lng: tp[0], Lat: tp[1]}
		if !GeomContainsPoint(g, p) {
			out = append(out, p)
			if d := distanceToGeomM(g, p); d > maxD {
				maxD = d
			}
		}
	}
	return out, maxD
}

func distanceToGeomM(g *Geom, p Point) float64 {
	const mPerDeg = 111320.0
	best := math.MaxFloat64
	for _, r := range g.Rings {
		n := len(r) - 1
		lat := p.Lat * math.Pi / 180
		for i := 0; i < n; i++ {
			a, b := r[i], r[(i+1)%n]
			d := pointSegDistanceM(p, a, b, lat, mPerDeg)
			if d < best {
				best = d
			}
		}
	}
	if best == math.MaxFloat64 {
		return 0
	}
	return best
}

func pointSegDistanceM(p, a, b Point, latRad float64, mPerDeg float64) float64 {
	ax := (a.Lng - p.Lng) * math.Cos(latRad) * mPerDeg
	ay := (a.Lat - p.Lat) * mPerDeg
	bx := (b.Lng - p.Lng) * math.Cos(latRad) * mPerDeg
	by := (b.Lat - p.Lat) * mPerDeg
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = -((ax*dx + ay*dy) / l2)
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	cx, cy := ax+t*dx, ay+t*dy
	return math.Hypot(cx, cy)
}

// ---------- notifications ----------

func (s *Store) listNotifications(u *userRow) ([]notificationView, error) {
	q := `SELECT id, type, title, body, permit_id, COALESCE(credential_id,''), created_at, COALESCE(ack.acked_at,'')
	      FROM notifications n
	      LEFT JOIN acknowledgements ack ON ack.notification_id=n.id AND ack.api_key=?
	      WHERE `
	var args []interface{}
	args = append(args, u.APIKey)
	switch u.Role {
	case "pilot":
		q += `n.aircraft_id=?`
		args = append(args, u.AircraftID)
	case "coordinator":
		q += `n.contractor_id=?`
		args = append(args, u.ContractorID)
	case "atc", "rescue":
		q += `1=1`
	default:
		return nil, fail(403, "unknown role")
	}
	q += ` ORDER BY n.created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notificationView{}
	for rows.Next() {
		var v notificationView
		if err := rows.Scan(&v.ID, &v.Type, &v.Title, &v.Body, &v.PermitID,
			&v.CredentialID, &v.CreatedAt, &v.AcknowledgedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) acknowledge(u *userRow, notifID string) (*notificationView, error) {
	var aircraft, contractor string
	err := s.db.QueryRow(
		`SELECT aircraft_id, contractor_id FROM notifications WHERE id=?`, notifID).
		Scan(&aircraft, &contractor)
	if err == sql.ErrNoRows {
		return nil, fail(404, "notification not found")
	}
	if err != nil {
		return nil, err
	}
	if u.Role == "pilot" && aircraft != u.AircraftID {
		return nil, fail(403, "not your notification")
	}
	if u.Role == "coordinator" && contractor != u.ContractorID {
		return nil, fail(403, "not your contractor's notification")
	}
	now := s.now().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO acknowledgements(notification_id, api_key, acked_at) VALUES(?,?,?)`,
		notifID, u.APIKey, now); err != nil {
		return nil, err
	}
	var v notificationView
	err = s.db.QueryRow(
		`SELECT n.id, n.type, n.title, n.body, n.permit_id, COALESCE(n.credential_id,''), n.created_at, COALESCE(ack.acked_at,'')
		 FROM notifications n
		 LEFT JOIN acknowledgements ack ON ack.notification_id=n.id AND ack.api_key=?
		 WHERE n.id=?`, u.APIKey, notifID).
		Scan(&v.ID, &v.Type, &v.Title, &v.Body, &v.PermitID, &v.CredentialID,
			&v.CreatedAt, &v.AcknowledgedAt)
	return &v, err
}
