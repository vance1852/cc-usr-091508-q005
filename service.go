package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------- errors ----------

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func fail(status int, msg string) error { return &apiError{Status: status, Msg: msg} }

// ---------- time window helpers ----------

func parseWindow(from, until string) (time.Time, time.Time, error) {
	f, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return time.Time{}, time.Time{}, fail(400, "validFrom must be RFC3339")
	}
	u, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return time.Time{}, time.Time{}, fail(400, "validUntil must be RFC3339")
	}
	if !u.After(f) {
		return time.Time{}, time.Time{}, fail(400, "validUntil must be after validFrom")
	}
	return f.UTC(), u.UTC(), nil
}

func windowsOverlap(f1, u1, f2, u2 time.Time) bool {
	return f1.Before(u2) && f2.Before(u1)
}

func altOverlap(min1, max1, min2, max2 float64) bool {
	return min1 <= max2 && min2 <= max1
}

// ---------- permit application ----------

func (s *Store) createPermit(u *userRow, dto createPermitDTO) (string, error) {
	if u.Role != "coordinator" {
		return "", fail(403, "only coordinators can file permit applications")
	}
	if dto.AircraftID == "" {
		return "", fail(400, "aircraftId is required")
	}
	ac, err := s.aircraft(dto.AircraftID)
	if err != nil {
		return "", fail(404, "unknown aircraft")
	}
	if ac.ContractorID != u.ContractorID {
		// Cross-contractor reference is refused, not merely hidden.
		return "", fail(403, "aircraft belongs to another contractor")
	}
	g, err := ParseGeom(dto.Polygon)
	if err != nil {
		return "", fail(400, err.Error())
	}
	from, until, err := parseWindow(dto.ValidFrom, dto.ValidUntil)
	if err != nil {
		return "", err
	}
	if dto.AltitudeMin < 0 || dto.AltitudeMax <= dto.AltitudeMin {
		return "", fail(400, "altitude band invalid")
	}
	if !capsSatisfied(ac.Capabilities, dto.RequiredCapabilities) {
		return "", fail(422, fmt.Sprintf(
			"aircraft %s capabilities %v do not cover required %v",
			ac.Callsign, ac.Capabilities, dto.RequiredCapabilities))
	}
	caps, _ := json.Marshal(dto.RequiredCapabilities)
	now := s.now().Format(time.RFC3339)
	permitID := newID("permit")
	versionID := newID("ver")

	err = s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO permits
		 (id, contractor_id, aircraft_id, mission_name, created_by, created_at, state,
		  valid_from, valid_until, alt_min, alt_max, required_capabilities)
		 VALUES(?,?,?,?,?,?, 'PENDING', ?,?,?,?,?)`,
			permitID, u.ContractorID, ac.ID, dto.MissionName, u.APIKey, now,
			from.Format(time.RFC3339), until.Format(time.RFC3339),
			dto.AltitudeMin, dto.AltitudeMax, string(caps))
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO permit_versions
		 (id, permit_id, seq, kind, source, polygon, alt_min, alt_max, valid_from, valid_until,
		  required_capabilities, decided_by, reason, created_at, active)
		 VALUES(?,?,1,'DRAFT','APPLICATION',?,?,?,?,?,?,?, ?,?, 0)`,
			versionID, permitID, string(g.Marshal()), dto.AltitudeMin, dto.AltitudeMax,
			from.Format(time.RFC3339), until.Format(time.RFC3339), string(caps),
			u.APIKey, "initial application", now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE permits SET current_version_id=? WHERE id=?`, versionID, permitID)
		if err != nil {
			return err
		}
		s.event(tx, permitID, "PERMIT_APPLIED", "APPLICATION", u.APIKey, dto.MissionName)
		return nil
	})
	if err != nil {
		return "", err
	}
	// Register spatial conflicts (no credentials exist yet, so nothing freezes).
	if cerr := s.withTx(func(tx *sql.Tx) error { return s.evaluateConflicts(tx) }); cerr != nil {
		return "", cerr
	}
	return permitID, nil
}

func capsSatisfied(have, required []string) bool {
	set := map[string]bool{}
	for _, c := range have {
		set[strings.ToLower(strings.TrimSpace(c))] = true
	}
	for _, c := range required {
		if c == "" {
			continue
		}
		if !set[strings.ToLower(strings.TrimSpace(c))] {
			return false
		}
	}
	return true
}

// ---------- ATC decision ----------

type decisionResult struct {
	PermitID       string          `json:"permitId"`
	State          string          `json:"state"`
	CurrentVersion any             `json:"currentVersion"`
	Credential     *credView       `json:"credential,omitempty"`
	Conflicts      []conflictView  `json:"conflicts,omitempty"`
	SuggestedSafe  json.RawMessage `json:"suggestedSafePolygon,omitempty"`
}

func (s *Store) decide(u *userRow, permitID string, dto decisionDTO) (*decisionResult, error) {
	if u.Role != "atc" {
		return nil, fail(403, "only ATC can decide on permits")
	}
	action := strings.ToLower(dto.Action)
	if action != "approve" && action != "shrink" && action != "deny" && action != "revoke" {
		return nil, fail(400, "action must be approve|shrink|deny|revoke")
	}

	permit, err := s.loadPermitCore(permitID)
	if err != nil {
		return nil, err
	}
	switch action {
	case "approve", "deny":
		if permit.state != "PENDING" {
			return nil, fail(409, "only pending applications can be "+action+"d")
		}
	case "shrink":
		if permit.state != "APPROVED" {
			return nil, fail(409, "only approved permits can be shrunk")
		}
	case "revoke":
		if permit.state == "REVOKED" {
			return nil, fail(409, "permit already revoked")
		}
	}

	var g *Geom
	if action == "approve" || action == "shrink" {
		if action == "shrink" {
			if len(dto.Polygon) == 0 {
				return nil, fail(400, "shrink requires a polygon")
			}
			g, err = ParseGeom(dto.Polygon)
			if err != nil {
				return nil, fail(400, err.Error())
			}
			if !geomWithin(g, mustParse(permit.currentPolygon)) {
				return nil, fail(422, "shrunk polygon must stay within the previously authorized area")
			}
		} else {
			g = mustParse(permit.currentPolygon)
		}

		//避让规则：不能压救援直升机航线，不能与其它现行许可在时空高度上重叠。
		blockers, hitConflicts, err := s.approvalBlockers(permit, g)
		if err != nil {
			return nil, err
		}
		if len(hitConflicts) > 0 {
			safe, _ := SuggestSafe(g, blockers)
			res := &decisionResult{
				PermitID:      permitID,
				State:         permit.state,
				Conflicts:     hitConflicts,
				SuggestedSafe: safe.Marshal(),
			}
			b, _ := json.Marshal(res)
			return nil, &apiError{Status: 422, Msg: string(b)}
		}
	}

	now := s.now()
	newVersionID := newID("ver")
	var issued *credentialRow

	err = s.withTx(func(tx *sql.Tx) error {
		seq := permit.seq + 1
		kind := map[string]string{
			"approve": "APPROVED", "shrink": "SHRUNK",
			"deny": "DENIED", "revoke": "REVOKED",
		}[action]
		state := map[string]string{
			"approve": "APPROVED", "shrink": "APPROVED",
			"deny": "DENIED", "revoke": "REVOKED",
		}[action]

		poly := permit.currentPolygon
		if action == "shrink" {
			poly = string(g.Marshal())
		}
		_, err := tx.Exec(
			`INSERT INTO permit_versions
			 (id, permit_id, seq, kind, source, polygon, alt_min, alt_max, valid_from, valid_until,
			  required_capabilities, decided_by, reason, created_at, active)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			newVersionID, permitID, seq, kind, "ATC_DECISION", poly,
			permit.altMin, permit.altMax, permit.validFrom, permit.validUntil,
			permit.requiredCaps, u.APIKey, dto.Reason, now.Format(time.RFC3339))
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE permit_versions SET active=0 WHERE permit_id=? AND id<>?`,
			permitID, newVersionID)
		if err != nil {
			return err
		}
		active := 0
		if state == "APPROVED" {
			active = 1
		}
		_, err = tx.Exec(`UPDATE permit_versions SET active=? WHERE id=?`, active, newVersionID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(
			`UPDATE permits SET state=?, current_version_id=? WHERE id=?`,
			state, newVersionID, permitID)
		if err != nil {
			return err
		}
		s.event(tx, permitID, "ATC_"+strings.ToUpper(action), "ATC_DECISION", u.APIKey, dto.Reason)

		// Every prior credential is single-use and bound to one version; a new
		// effective version supersedes (shrink) or revokes them before issuing.
		if state == "APPROVED" || action == "revoke" {
			reason := "SUPERSEDED"
			notType := "PERMIT_SHRUNK"
			title := "授权范围已缩小，原凭证立即冻结"
			if action == "revoke" {
				reason = "REVOKED"
				notType = "PERMIT_REVOKED"
				title = "授权已撤销，凭证立即冻结"
			} else if seq == 2 && permit.state == "PENDING" {
				reason = "" // first approval: nothing to freeze
			}
			if reason != "" {
				if err := s.freezeCredentials(tx, permitID, "", reason, notType, title, dto.Reason); err != nil {
					return err
				}
			}
		}
		if state == "APPROVED" {
			c := &credentialRow{
				ID:           newID("crd"),
				PermitID:     permitID,
				VersionID:    newVersionID,
				AircraftID:   permit.aircraftID,
				ContractorID: permit.contractorID,
				Token:        newToken(),
				Status:       "ISSUED",
				IssuedAt:     now.Format(time.RFC3339),
			}
			_, err = tx.Exec(`INSERT INTO credentials
			 (id, permit_id, version_id, aircraft_id, contractor_id, token, status, issued_at)
			 VALUES(?,?,?,?,?,?, 'ISSUED', ?)`,
				c.ID, c.PermitID, c.VersionID, c.AircraftID, c.ContractorID, c.Token, c.IssuedAt)
			if err != nil {
				return err
			}
			issued = c
			s.notify(tx, c.AircraftID, c.ContractorID, c.ID, permitID,
				"CREDENTIAL_ISSUED", "新飞行凭证已签发",
				fmt.Sprintf("任务 %s 的一次性凭证已签发，版本 v%d。", permit.missionName, seq))
			s.event(tx, permitID, "CREDENTIAL_ISSUED", "ATC_DECISION", u.APIKey, c.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Recompute spatial conflicts after the decision; the engine freezes any
	// credentials whose ranges now intersect (defensive, across all permits).
	if err := s.withTx(func(tx *sql.Tx) error { return s.evaluateConflicts(tx) }); err != nil {
		return nil, err
	}

	detail, err := s.permitDetail(permitID, u)
	if err != nil {
		return nil, err
	}
	res := &decisionResult{
		PermitID:       permitID,
		State:          detail["state"].(string),
		CurrentVersion: detail["currentVersion"].(versionView),
		Conflicts:      detail["conflicts"].([]conflictView),
	}
	if issued != nil {
		// Re-read in case the engine just froze it.
		for _, c := range detail["credentials"].([]credView) {
			if c.ID == issued.ID {
				cc := c
				res.Credential = &cc
			}
		}
	}
	return res, nil
}

func geomWithin(inner, outer *Geom) bool {
	for _, r := range inner.Rings {
		for _, p := range r {
			if !GeomContainsPoint(outer, p) {
				return false
			}
		}
	}
	return true
}

func mustParse(s string) *Geom {
	g, err := ParseGeom(json.RawMessage(s))
	if err != nil {
		panic(err)
	}
	return g
}

// approvalBlockers returns blocker geometries (for safe-area suggestion) and
// human-facing conflicts against active routes and other live approvals.
func (s *Store) approvalBlockers(p *permitCore, g *Geom) ([]*Geom, []conflictView, error) {
	var blockers []*Geom
	var views []conflictView

	routes, err := s.activeRoutes()
	if err != nil {
		return nil, nil, err
	}
	for _, rt := range routes {
		if pts := IntersectionPoints(g, rt.geom); len(pts) > 0 {
			blockers = append(blockers, rt.geom)
			views = append(views, conflictView{
				Kind:        "PERMIT_ROUTE",
				Route:       &routeRef{ID: rt.id, Name: rt.name, Version: rt.version},
				Description: fmt.Sprintf("与救援航线 %s（v%d）走廊相交", rt.name, rt.version),
				Points:      pts,
			})
		}
	}
	others, err := s.livePermits(p.id)
	if err != nil {
		return nil, nil, err
	}
	for _, o := range others {
		if !windowsOverlap(p.from, p.until, o.from, o.until) ||
			!altOverlap(p.altMin, p.altMax, o.altMin, o.altMax) {
			continue
		}
		if pts := IntersectionPoints(g, o.geom); len(pts) > 0 {
			blockers = append(blockers, o.geom)
			views = append(views, conflictView{
				Kind: "PERMIT_PERMIT",
				OtherPermit: &otherPermitRef{
					ID: o.id, AircraftID: o.aircraftID, MissionName: o.missionName,
				},
				Description: fmt.Sprintf("与任务 %s（%s）现行授权范围相交", o.missionName, o.aircraftID),
				Points:      pts,
			})
		}
	}
	return blockers, views, nil
}

// ---------- freeze engine ----------

type querier interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
	Exec(query string, args ...interface{}) (sql.Result, error)
}

type routeCore struct {
	id      string
	name    string
	version int
	geom    *Geom
}

func (s *Store) activeRoutes() ([]routeCore, error) {
	return queryActiveRoutes(s.db)
}

func queryActiveRoutes(q querier) ([]routeCore, error) {
	rows, err := q.Query(
		`SELECT id, name, version, corridor FROM rescue_routes WHERE active=1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routeCore
	for rows.Next() {
		var rc routeCore
		var poly string
		if err := rows.Scan(&rc.id, &rc.name, &rc.version, &poly); err != nil {
			return nil, err
		}
		g, err := ParseGeom(json.RawMessage(poly))
		if err != nil {
			return nil, err
		}
		rc.geom = g
		out = append(out, rc)
	}
	return out, nil
}

type permitCore struct {
	id             string
	contractorID   string
	aircraftID     string
	missionName    string
	state          string
	seq            int
	currentPolygon string
	altMin         float64
	altMax         float64
	validFrom      string
	validUntil     string
	requiredCaps   string
	from, until    time.Time
	geom           *Geom
}

func (s *Store) loadPermitCore(id string) (*permitCore, error) {
	var p permitCore
	err := s.db.QueryRow(
		`SELECT p.id, p.contractor_id, p.aircraft_id, p.mission_name, p.state,
		        pv.seq, pv.polygon, p.alt_min, p.alt_max,
		        p.valid_from, p.valid_until, p.required_capabilities
		 FROM permits p JOIN permit_versions pv ON pv.id = p.current_version_id
		 WHERE p.id=?`, id).
		Scan(&p.id, &p.contractorID, &p.aircraftID, &p.missionName, &p.state,
			&p.seq, &p.currentPolygon, &p.altMin, &p.altMax,
			&p.validFrom, &p.validUntil, &p.requiredCaps)
	if err == sql.ErrNoRows {
		return nil, fail(404, "permit not found")
	}
	if err != nil {
		return nil, err
	}
	p.from, _ = time.Parse(time.RFC3339, p.validFrom)
	p.until, _ = time.Parse(time.RFC3339, p.validUntil)
	p.geom = mustParse(p.currentPolygon)
	return &p, nil
}

// livePermits loads other permits that currently occupy airspace: APPROVED,
// within their time window, with an active version.
func (s *Store) livePermits(excludeID string) ([]*permitCore, error) {
	return queryLivePermits(s.db, excludeID, s.now())
}

const livePermitsSQL = `
 SELECT p.id, p.contractor_id, p.aircraft_id, p.mission_name, p.state,
        pv.seq, pv.polygon, p.alt_min, p.alt_max,
        p.valid_from, p.valid_until, p.required_capabilities
 FROM permits p JOIN permit_versions pv ON pv.id = p.current_version_id
 WHERE p.state='APPROVED' AND pv.active=1 AND p.id<>?
 ORDER BY p.created_at`

func queryLivePermits(q querier, excludeID string, now time.Time) ([]*permitCore, error) {
	rows, err := q.Query(livePermitsSQL, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*permitCore
	for rows.Next() {
		var p permitCore
		if err := rows.Scan(&p.id, &p.contractorID, &p.aircraftID, &p.missionName, &p.state,
			&p.seq, &p.currentPolygon, &p.altMin, &p.altMax,
			&p.validFrom, &p.validUntil, &p.requiredCaps); err != nil {
			return nil, err
		}
		p.from, _ = time.Parse(time.RFC3339, p.validFrom)
		p.until, _ = time.Parse(time.RFC3339, p.validUntil)
		if now.Before(p.from) || now.After(p.until) {
			continue
		}
		p.geom = mustParse(p.currentPolygon)
		out = append(out, &p)
	}
	return out, nil
}

// allConsideredPermits loads PENDING + APPROVED permits still in their window
// (PENDING never freeze; they only generate conflict markers for ATC).
func (s *Store) allConsideredPermits() ([]*permitCore, error) {
	return queryConsideredPermits(s.db, s.now())
}

const consideredPermitsSQL = `
 SELECT p.id, p.contractor_id, p.aircraft_id, p.mission_name, p.state,
        pv.seq, pv.polygon, p.alt_min, p.alt_max,
        p.valid_from, p.valid_until, p.required_capabilities
 FROM permits p JOIN permit_versions pv ON pv.id = p.current_version_id
 WHERE p.state IN ('PENDING','APPROVED')
 ORDER BY p.created_at`

func queryConsideredPermits(q querier, now time.Time) ([]*permitCore, error) {
	rows, err := q.Query(consideredPermitsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*permitCore
	for rows.Next() {
		var p permitCore
		if err := rows.Scan(&p.id, &p.contractorID, &p.aircraftID, &p.missionName, &p.state,
			&p.seq, &p.currentPolygon, &p.altMin, &p.altMax,
			&p.validFrom, &p.validUntil, &p.requiredCaps); err != nil {
			return nil, err
		}
		p.from, _ = time.Parse(time.RFC3339, p.validFrom)
		p.until, _ = time.Parse(time.RFC3339, p.validUntil)
		if now.After(p.until) {
			continue
		}
		p.geom = mustParse(p.currentPolygon)
		out = append(out, &p)
	}
	return out, nil
}

type conflictKey struct {
	kind             string
	permitA, permitB string
	routeID          string
	routeVersion     int
}

// evaluateConflicts rebuilds the open-conflict picture and freezes every
// ISSUED credential touched by a conflict. Freeze is sticky: once a credential
// is frozen it stays frozen even if the geometry later separates — ATC must
// shrink/re-approve, which issues a fresh credential.
func (s *Store) evaluateConflicts(tx *sql.Tx) error {
	routes, err := queryActiveRoutes(tx)
	if err != nil {
		return err
	}
	permits, err := queryConsideredPermits(tx, s.now())
	if err != nil {
		return err
	}
	now := s.now()

	type desired struct {
		key     conflictKey
		desc    string
		points  []Point
		permitA string
	}
	var want []desired
	wantKeys := map[conflictKey]bool{}

	// permit vs active rescue-route corridor
	for _, p := range permits {
		for _, rt := range routes {
			if pts := IntersectionPoints(p.geom, rt.geom); len(pts) > 0 {
				k := conflictKey{"PERMIT_ROUTE", p.id, "", rt.id, rt.version}
				want = append(want, desired{
					key: k, permitA: p.id, points: pts,
					desc: fmt.Sprintf("与救援航线 %s（v%d）走廊相交于 %d 处", rt.name, rt.version, len(pts)),
				})
				wantKeys[k] = true
			}
		}
	}
	// permit vs permit (time window + altitude band + polygon)
	for i := 0; i < len(permits); i++ {
		for j := i + 1; j < len(permits); j++ {
			a, b := permits[i], permits[j]
			if !windowsOverlap(a.from, a.until, b.from, b.until) ||
				!altOverlap(a.altMin, a.altMax, b.altMin, b.altMax) {
				continue
			}
			if pts := IntersectionPoints(a.geom, b.geom); len(pts) > 0 {
				k := conflictKey{"PERMIT_PERMIT", a.id, b.id, "", 0}
				want = append(want, desired{
					key: k, permitA: a.id, points: pts,
					desc: fmt.Sprintf("任务 %s 与 %s 授权范围相交于 %d 处",
						a.missionName, b.missionName, len(pts)),
				})
				wantKeys[k] = true
			}
		}
	}

	// Insert missing conflicts.
	for _, w := range want {
		var existing string
		var oldVersion int
		if w.key.kind == "PERMIT_ROUTE" {
			err = tx.QueryRow(
				`SELECT id, COALESCE(route_version,0) FROM conflicts
				 WHERE kind='PERMIT_ROUTE' AND permit_a=? AND route_id=? AND resolved_at IS NULL`,
				w.permitA, w.key.routeID).Scan(&existing, &oldVersion)
		} else {
			err = tx.QueryRow(
				`SELECT id, 0 FROM conflicts
				 WHERE kind='PERMIT_PERMIT' AND permit_a=? AND permit_b=? AND resolved_at IS NULL`,
				w.permitA, w.key.permitB).Scan(&existing, &oldVersion)
		}
		if err == nil {
			if w.key.kind == "PERMIT_ROUTE" && oldVersion != w.key.routeVersion {
				_, _ = tx.Exec(`UPDATE conflicts SET resolved_at=? WHERE id=?`,
					now.Format(time.RFC3339), existing)
				// fall through and insert against the new version
			} else {
				pts, _ := json.Marshal(w.points)
				_, _ = tx.Exec(`UPDATE conflicts SET points=?, description=? WHERE id=?`,
					string(pts), w.desc, existing)
				continue
			}
		} else if err != sql.ErrNoRows {
			return err
		}

		pts, _ := json.Marshal(w.points)
		cid := newID("cnf")
		var permitB, routeID interface{}
		var routeVersion interface{}
		if w.key.permitB != "" {
			permitB = w.key.permitB
		}
		if w.key.routeID != "" {
			routeID = w.key.routeID
			routeVersion = w.key.routeVersion
		}
		_, err = tx.Exec(`INSERT INTO conflicts
		 (id, kind, permit_a, permit_b, route_id, route_version, description, points, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
			cid, w.key.kind, w.permitA, permitB, routeID, routeVersion,
			w.desc, string(pts), now.Format(time.RFC3339))
		if err != nil {
			return err
		}
		// Record conflict on both permits' event streams.
		s.event(tx, w.permitA, "CONFLICT_DETECTED", "FREEZE_ENGINE", "", w.desc)
		if w.key.permitB != "" {
			s.event(tx, w.key.permitB, "CONFLICT_DETECTED", "FREEZE_ENGINE", "", w.desc)
		}
	}

	// Resolve open conflicts whose condition no longer holds.
	rows, err := tx.Query(
		`SELECT id, kind, permit_a, COALESCE(permit_b,''), COALESCE(route_id,''), COALESCE(route_version,0)
		 FROM conflicts WHERE resolved_at IS NULL`)
	if err != nil {
		return err
	}
	type openRow struct {
		id, kind, a, b, route string
		routeVersion          int
	}
	var opens []openRow
	for rows.Next() {
		var o openRow
		if err := rows.Scan(&o.id, &o.kind, &o.a, &o.b, &o.route, &o.routeVersion); err != nil {
			rows.Close()
			return err
		}
		opens = append(opens, o)
	}
	rows.Close()

	liveByID := map[string]*permitCore{}
	for _, p := range permits {
		liveByID[p.id] = p
	}
	routeByID := map[string]routeCore{}
	for _, rt := range routes {
		routeByID[rt.id] = rt
	}
	for _, o := range opens {
		alive := false
		a, okA := liveByID[o.a]
		_, okB := liveByID[o.b]
		if o.kind == "PERMIT_ROUTE" {
			okB = o.b == ""
		}
		if okA && okB {
			if o.kind == "PERMIT_ROUTE" {
				if rt, ok := routeByID[o.route]; ok && rt.version == o.routeVersion {
					alive = len(IntersectionPoints(a.geom, rt.geom)) > 0
				}
			} else if bp, ok := liveByID[o.b]; ok {
				alive = windowsOverlap(a.from, a.until, bp.from, bp.until) &&
					altOverlap(a.altMin, a.altMax, bp.altMin, bp.altMax) &&
					len(IntersectionPoints(a.geom, bp.geom)) > 0
			}
		}
		if !alive {
			_, _ = tx.Exec(`UPDATE conflicts SET resolved_at=? WHERE id=?`,
				now.Format(time.RFC3339), o.id)
		}
	}

	// Freeze credentials of every permit with an open conflict.
	affected, err := openConflictPermits(tx)
	if err != nil {
		return err
	}
	for permitID, reasons := range affected {
		reason, title, body, typ := reasons.freezeText()
		// Empty status filter: ISSUED credentials freeze, REDEEMED (in-flight)
		// ones get the immediate avoidance warning — see freezeCredentials.
		if err := s.freezeCredentials(tx, permitID, "", reason, typ, title, body); err != nil {
			return err
		}
	}
	return nil
}

type freezeReasons struct {
	route   bool
	overlap bool
}

func (r freezeReasons) freezeText() (reason, title, body, typ string) {
	switch {
	case r.route && r.overlap:
		return "CONFLICT", "空域冲突：凭证已立即冻结",
			"授权范围与救援航线及其他任务相交，凭证立即冻结，等待空管重新裁决。", "PERMIT_CONFLICT"
	case r.route:
		return "RESCUE_ROUTE", "救援航线更新：凭证已立即冻结",
			"授权范围与最新救援直升机航线相交，避让规则触发，凭证立即冻结。", "RESCUE_ROUTE_UPDATE"
	default:
		return "RANGE_OVERLAP", "任务范围相交：凭证已立即冻结",
			"授权范围与其他现行任务相交，凭证立即冻结，等待空管重新裁决。", "PERMIT_CONFLICT"
	}
}

func openConflictPermits(tx *sql.Tx) (map[string]freezeReasons, error) {
	out := map[string]freezeReasons{}
	// A PENDING application freezes nobody: it holds no credential, and an
	// approved neighbor must not be grounded by an application that may never
	// be approved. Permit-permit freezes therefore require both sides live.
	rows, err := tx.Query(
		`SELECT c.kind, c.permit_a, pa.state, COALESCE(c.permit_b,''), COALESCE(pb.state,'')
		 FROM conflicts c
		 JOIN permits pa ON pa.id=c.permit_a
		 LEFT JOIN permits pb ON pb.id=c.permit_b
		 WHERE c.resolved_at IS NULL
		   AND (c.kind='PERMIT_ROUTE' AND pa.state='APPROVED'
		        OR c.kind='PERMIT_PERMIT' AND pa.state='APPROVED' AND pb.state='APPROVED')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, a, sa, b, sb string
		if err := rows.Scan(&kind, &a, &sa, &b, &sb); err != nil {
			return nil, err
		}
		r := out[a]
		if kind == "PERMIT_ROUTE" {
			r.route = true
		} else {
			r.overlap = true
		}
		out[a] = r
		if b != "" {
			r2 := out[b]
			r2.overlap = true
			out[b] = r2
		}
	}
	return out, nil
}

// freezeCredentials holds unused credentials: ISSUED ones become FROZEN;
// already-REDEEMED aircraft are in the air and are not "frozen" retroactively —
// they get an immediate in-flight warning and must truthfully record whatever
// avoidance deviation results (segments stay append-only).
func (s *Store) freezeCredentials(tx *sql.Tx, permitID, onlyStatus, reason, notType, title, body string) error {
	q := `SELECT id, aircraft_id, contractor_id, status FROM credentials
	      WHERE permit_id=? AND status <> 'FROZEN'`
	args := []interface{}{permitID}
	if onlyStatus != "" {
		q += ` AND status=?`
		args = append(args, onlyStatus)
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return err
	}
	type target struct{ id, aircraft, contractor, status string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.aircraft, &t.contractor, &t.status); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	now := s.now().Format(time.RFC3339)
	for _, t := range ts {
		if t.status == "ISSUED" {
			if _, err := tx.Exec(
				`UPDATE credentials SET status='FROZEN', freeze_reason=?, frozen_at=? WHERE id=?`,
				reason, now, t.id); err != nil {
				return err
			}
			s.notify(tx, t.aircraft, t.contractor, t.id, permitID, notType, title, body)
			s.event(tx, permitID, "CREDENTIAL_FROZEN", "FREEZE_ENGINE", "",
				fmt.Sprintf("%s: %s", t.id, reason))
		} else if t.status == "REDEEMED" {
			// Avoid duplicate avoidance notices on repeated conflict re-evaluation.
			var prior int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM notifications
				 WHERE credential_id=? AND type='INFLIGHT_AVOIDANCE'`, t.id).Scan(&prior); err != nil {
				return err
			}
			if prior > 0 {
				continue
			}
			s.notify(tx, t.aircraft, t.contractor, t.id, permitID,
				"INFLIGHT_AVOIDANCE", "飞行中空域变更：立即按避让预案处置并如实报备航迹",
				"你的凭证已在使用中；新的空域风险（"+reason+"）要求立即避让，航段将按实际航迹登记偏航，不得回写为未执行。")
			s.event(tx, permitID, "INFLIGHT_AVOIDANCE_NOTIFIED", "FREEZE_ENGINE", "",
				fmt.Sprintf("%s: %s", t.id, reason))
		}
	}
	return nil
}

// ---------- notifications ----------

func (s *Store) notify(tx *sql.Tx, aircraftID, contractorID, credentialID, permitID,
	notType, title, body string) {
	var cred interface{}
	if credentialID != "" {
		cred = credentialID
	}
	_, _ = tx.Exec(`INSERT INTO notifications
	 (id, aircraft_id, contractor_id, credential_id, permit_id, type, title, body, created_at)
	 VALUES(?,?,?,?,?,?,?,?,?)`,
		newID("ntf"), aircraftID, contractorID, cred, permitID,
		notType, title, body, s.now().Format(time.RFC3339))
}

// ---------- link loss ----------

func (s *Store) reportLink(u *userRow, aircraftID, status string) error {
	if status != "ok" && status != "lost" {
		return fail(400, "status must be ok|lost")
	}
	ac, err := s.aircraft(aircraftID)
	if err != nil {
		return fail(404, "unknown aircraft")
	}
	if u.Role == "coordinator" && ac.ContractorID != u.ContractorID {
		return fail(403, "aircraft belongs to another contractor")
	}
	if u.Role == "pilot" && ac.ID != u.AircraftID {
		return fail(403, "not your aircraft")
	}
	now := s.now().Format(time.RFC3339)
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE aircraft SET link_status=?, last_seen_at=? WHERE id=?`,
			status, now, aircraftID)
		if err != nil {
			return err
		}
		s.event(tx, "", "AIRCRAFT_LINK_"+strings.ToUpper(status), "LINK_REPORT", u.APIKey, aircraftID)
		if status == "lost" {
			// All usable credentials of this aircraft freeze immediately.
			rows, err := tx.Query(
				`SELECT c.id, c.permit_id FROM credentials c
				 WHERE c.aircraft_id=? AND c.status='ISSUED'`, aircraftID)
			if err != nil {
				return err
			}
			type pair struct{ cred, permit string }
			var ps []pair
			for rows.Next() {
				var p pair
				if err := rows.Scan(&p.cred, &p.permit); err != nil {
					rows.Close()
					return err
				}
				ps = append(ps, p)
			}
			rows.Close()
			for _, p := range ps {
				if _, err := tx.Exec(
					`UPDATE credentials SET status='FROZEN', freeze_reason='LINK_LOST', frozen_at=? WHERE id=?`,
					now, p.cred); err != nil {
					return err
				}
				s.notify(tx, aircraftID, ac.ContractorID, p.cred, p.permit,
					"AIRCRAFT_LINK_LOST", "飞行器失联：凭证已立即冻结",
					"收到失联报告，待命不得起飞；恢复链路后仍需等待空管重新签发凭证。")
				s.event(tx, p.permit, "CREDENTIAL_FROZEN", "LINK_REPORT", u.APIKey,
					p.cred+": LINK_LOST")
			}
			// Holders of in-flight (redeemed) credentials get the warning too;
			// their segment truthfully records the resulting deviation.
			rows2, err := tx.Query(
				`SELECT c.id, c.permit_id FROM credentials c
				 WHERE c.aircraft_id=? AND c.status='REDEEMED'
				 AND NOT EXISTS(SELECT 1 FROM segments sg WHERE sg.credential_id=c.id AND sg.ended_at IS NOT NULL)`,
				aircraftID)
			if err != nil {
				return err
			}
			var inflight []pair
			for rows2.Next() {
				var p pair
				if err := rows2.Scan(&p.cred, &p.permit); err != nil {
					rows2.Close()
					return err
				}
				inflight = append(inflight, p)
			}
			rows2.Close()
			for _, p := range inflight {
				s.notify(tx, aircraftID, ac.ContractorID, p.cred, p.permit,
					"AIRCRAFT_LINK_LOST_INFLIGHT", "飞行中失联：按预案处置并如实登记偏航",
					"恢复后必须报备航迹与处置结果，航段记录不得回写为未执行。")
			}
		}
		return nil
	})
}
