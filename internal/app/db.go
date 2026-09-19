package app

import (
	"database/sql"
	"encoding/json"
	"time"

	"airspace/internal/store"
)

// dbtx is satisfied by both *sql.DB and *sql.Tx so every helper can run
// inside or outside a transaction.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTimeStr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.FmtTime(*t)
}

func scanNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := store.ParseTime(ns.String)
	return &t
}

func (s *Server) userByToken(q dbtx, token string) (*store.User, error) {
	row := q.QueryRow(`SELECT id, name, role, contractor_id, token FROM users WHERE token = ?`, token)
	var u store.User
	var cid sql.NullInt64
	if err := row.Scan(&u.ID, &u.Name, &u.Role, &cid, &u.Token); err != nil {
		return nil, err
	}
	if cid.Valid {
		u.ContractorID = &cid.Int64
	}
	return &u, nil
}

func (s *Server) loadRequest(q dbtx, id int64) (*store.Request, error) {
	row := q.QueryRow(`SELECT id, requester_id, contractor_id, area, valid_from, valid_to, capabilities, purpose, created_at
		FROM requests WHERE id = ?`, id)
	var r store.Request
	var area, caps string
	var vf, vt, ca string
	if err := row.Scan(&r.ID, &r.RequesterID, &r.ContractorID, &area, &vf, &vt, &caps, &r.Purpose, &ca); err != nil {
		return nil, err
	}
	r.Area = store.UnmarshalPolygon(area)
	r.ValidFrom, r.ValidTo, r.CreatedAt = store.ParseTime(vf), store.ParseTime(vt), store.ParseTime(ca)
	r.Capabilities = json.RawMessage(caps)
	return &r, nil
}

func (s *Server) loadVersions(q dbtx, requestID int64) ([]store.Version, error) {
	rows, err := q.Query(`SELECT id, request_id, version, polygon, valid_from, valid_to,
		decision, decided_by, decision_source, reason, created_at
		FROM auth_versions WHERE request_id = ? ORDER BY version`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Version
	for rows.Next() {
		var v store.Version
		var poly, vf, vt, ca string
		if err := rows.Scan(&v.ID, &v.RequestID, &v.Version, &poly, &vf, &vt,
			&v.Decision, &v.DecidedBy, &v.DecisionSource, &v.Reason, &ca); err != nil {
			return nil, err
		}
		v.Polygon = store.UnmarshalPolygon(poly)
		v.ValidFrom, v.ValidTo, v.CreatedAt = store.ParseTime(vf), store.ParseTime(vt), store.ParseTime(ca)
		out = append(out, v)
	}
	return out, rows.Err()
}

// latestVersion returns the single currently valid version of a request,
// or nil when no decision exists yet.
func (s *Server) latestVersion(q dbtx, requestID int64) (*store.Version, error) {
	vs, err := s.loadVersions(q, requestID)
	if err != nil || len(vs) == 0 {
		return nil, err
	}
	return &vs[len(vs)-1], nil
}

func (s *Server) loadCredentials(q dbtx, requestID int64) ([]store.Credential, error) {
	rows, err := q.Query(`SELECT id, version_id, request_id, aircraft_id, pilot_id, token, status,
		issued_at, used_at, frozen_at, frozen_reason
		FROM credentials WHERE request_id = ? ORDER BY id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCredentials(rows)
}

func scanCredentials(rows *sql.Rows) ([]store.Credential, error) {
	defer rows.Close()
	var out []store.Credential
	for rows.Next() {
		var c store.Credential
		var issued string
		var used, frozen sql.NullString
		if err := rows.Scan(&c.ID, &c.VersionID, &c.RequestID, &c.AircraftID, &c.PilotID,
			&c.Token, &c.Status, &issued, &used, &frozen, &c.FrozenReason); err != nil {
			return nil, err
		}
		c.IssuedAt = store.ParseTime(issued)
		c.UsedAt = scanNullTime(used)
		c.FrozenAt = scanNullTime(frozen)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) requestAircraft(q dbtx, requestID int64) ([]store.RequestAircraft, error) {
	rows, err := q.Query(`SELECT request_id, aircraft_id, pilot_id FROM request_aircraft WHERE request_id = ?`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.RequestAircraft
	for rows.Next() {
		var ra store.RequestAircraft
		if err := rows.Scan(&ra.RequestID, &ra.AircraftID, &ra.PilotID); err != nil {
			return nil, err
		}
		out = append(out, ra)
	}
	return out, rows.Err()
}

func (s *Server) loadAircraft(q dbtx, id int64) (*store.Aircraft, error) {
	row := q.QueryRow(`SELECT id, callsign, contractor_id, capabilities, link_status, updated_at FROM aircraft WHERE id = ?`, id)
	var a store.Aircraft
	var caps, ua string
	if err := row.Scan(&a.ID, &a.Callsign, &a.ContractorID, &caps, &a.LinkStatus, &ua); err != nil {
		return nil, err
	}
	a.Capabilities = json.RawMessage(caps)
	a.UpdatedAt = store.ParseTime(ua)
	return &a, nil
}

func activeRoutes(q dbtx) ([]store.RescueRoute, error) {
	rows, err := q.Query(`SELECT id, name, corridor, active, version, updated_by, created_at, updated_at
		FROM rescue_routes WHERE active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.RescueRoute
	for rows.Next() {
		var r store.RescueRoute
		var corridor, ca, ua string
		var active int
		var ub sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Name, &corridor, &active, &r.Version, &ub, &ca, &ua); err != nil {
			return nil, err
		}
		r.Corridor = store.UnmarshalPolygon(corridor)
		r.Active = active == 1
		if ub.Valid {
			r.UpdatedBy = &ub.Int64
		}
		r.CreatedAt, r.UpdatedAt = store.ParseTime(ca), store.ParseTime(ua)
		out = append(out, r)
	}
	return out, rows.Err()
}

// activeVersionView joins the current version of every request with the
// request itself, keeping only versions whose decision is in force.
type activeVersionView struct {
	Version      store.Version
	ContractorID int64
}

// currentActiveVersions returns the latest version of every request whose
// latest decision is approved or shrunk (i.e. currently authorizing).
func currentActiveVersions(q dbtx) ([]activeVersionView, error) {
	rows, err := q.Query(`SELECT v.id, v.request_id, v.version, v.polygon, v.valid_from, v.valid_to,
		v.decision, v.decided_by, v.decision_source, v.reason, v.created_at, r.contractor_id
		FROM auth_versions v
		JOIN (SELECT request_id, MAX(version) mv FROM auth_versions GROUP BY request_id) t
		  ON t.request_id = v.request_id AND t.mv = v.version
		JOIN requests r ON r.id = v.request_id
		WHERE v.decision IN ('approved','shrunk')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activeVersionView
	for rows.Next() {
		var av activeVersionView
		var poly, vf, vt, ca string
		if err := rows.Scan(&av.Version.ID, &av.Version.RequestID, &av.Version.Version, &poly, &vf, &vt,
			&av.Version.Decision, &av.Version.DecidedBy, &av.Version.DecisionSource,
			&av.Version.Reason, &ca, &av.ContractorID); err != nil {
			return nil, err
		}
		av.Version.Polygon = store.UnmarshalPolygon(poly)
		av.Version.ValidFrom = store.ParseTime(vf)
		av.Version.ValidTo = store.ParseTime(vt)
		av.Version.CreatedAt = store.ParseTime(ca)
		out = append(out, av)
	}
	return out, rows.Err()
}

// notify inserts an acknowledgable notification inside a transaction.
func notify(q dbtx, userID int64, requestID, credentialID *int64, kind, message string, now time.Time) error {
	_, err := q.Exec(`INSERT INTO notifications(user_id, request_id, credential_id, kind, message, created_at)
		VALUES (?,?,?,?,?,?)`, userID, nullInt64(requestID), nullInt64(credentialID), kind, message, store.FmtTime(now))
	return err
}

func contractorName(q dbtx, id int64) string {
	var name string
	if err := q.QueryRow(`SELECT name FROM contractors WHERE id = ?`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

func userName(q dbtx, id int64) string {
	var name string
	if err := q.QueryRow(`SELECT name FROM users WHERE id = ?`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}
