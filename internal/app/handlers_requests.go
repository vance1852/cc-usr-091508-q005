package app

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"airspace/internal/geo"
	"airspace/internal/store"

	"github.com/labstack/echo/v4"
)

// ---------- JSON views ----------

type polygonJSON = geo.Polygon

type requestView struct {
	ID           int64           `json:"id"`
	RequesterID  int64           `json:"requester_id"`
	ContractorID int64           `json:"contractor_id"`
	Contractor   string          `json:"contractor"`
	Area         polygonJSON     `json:"area"`
	ValidFrom    time.Time       `json:"valid_from"`
	ValidTo      time.Time       `json:"valid_to"`
	Capabilities json.RawMessage `json:"capabilities"`
	Purpose      string          `json:"purpose"`
	CreatedAt    time.Time       `json:"created_at"`
	Aircraft     []assignView    `json:"aircraft"`
}

type assignView struct {
	AircraftID int64  `json:"aircraft_id"`
	Callsign   string `json:"callsign"`
	PilotID    int64  `json:"pilot_id"`
	PilotName  string `json:"pilot_name"`
}

type versionView struct {
	ID             int64       `json:"id"`
	Version        int         `json:"version"`
	Decision       string      `json:"decision"`
	DecisionSource string      `json:"decision_source"`
	Reason         string      `json:"reason"`
	Polygon        polygonJSON `json:"polygon"`
	ValidFrom      time.Time   `json:"valid_from"`
	ValidTo        time.Time   `json:"valid_to"`
	DecidedBy      int64       `json:"decided_by"`
	DecidedByName  string      `json:"decided_by_name"`
	CreatedAt      time.Time   `json:"created_at"`
}

type credentialView struct {
	ID           int64      `json:"id"`
	VersionID    int64      `json:"version_id"`
	AircraftID   int64      `json:"aircraft_id"`
	Callsign     string     `json:"callsign"`
	PilotID      int64      `json:"pilot_id"`
	PilotName    string     `json:"pilot_name"`
	Status       string     `json:"status"`
	Token        string     `json:"token,omitempty"`
	IssuedAt     time.Time  `json:"issued_at"`
	UsedAt       *time.Time `json:"used_at,omitempty"`
	FrozenAt     *time.Time `json:"frozen_at,omitempty"`
	FrozenReason string     `json:"frozen_reason,omitempty"`
}

type pendingAckView struct {
	NotificationID int64     `json:"notification_id"`
	UserID         int64     `json:"user_id"`
	UserName       string    `json:"user_name"`
	Kind           string    `json:"kind"`
	CreatedAt      time.Time `json:"created_at"`
}

type segmentView struct {
	ID              int64      `json:"id"`
	CredentialID    int64      `json:"credential_id"`
	RequestID       int64      `json:"request_id"`
	AircraftID      int64      `json:"aircraft_id"`
	Callsign        string     `json:"callsign"`
	PilotID         int64      `json:"pilot_id"`
	PilotName       string     `json:"pilot_name"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	Deviated        bool       `json:"deviated"`
	DeviationReason string     `json:"deviation_reason,omitempty"`
	DeviationAction string     `json:"deviation_action,omitempty"`
	ReportedAt      time.Time  `json:"reported_at"`
}

// ---------- helpers ----------

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cred-" + hex.EncodeToString(b)
}

func parseID(c echo.Context, name string) (int64, error) {
	return strconv.ParseInt(c.Param(name), 10, 64)
}

func parseTimeParam(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

func (s *Server) canViewRequest(u *store.User, r *store.Request) bool {
	switch u.Role {
	case store.RoleATC:
		return true
	case store.RoleCoordinator:
		return u.ContractorID != nil && *u.ContractorID == r.ContractorID
	case store.RolePilot:
		row := s.db.QueryRow(`SELECT COUNT(*) FROM request_aircraft WHERE request_id = ? AND pilot_id = ?`, r.ID, u.ID)
		var n int
		if err := row.Scan(&n); err == nil && n > 0 {
			return true
		}
	}
	return false
}

func (s *Server) buildRequestView(q dbtx, r *store.Request) requestView {
	v := requestView{
		ID:           r.ID,
		RequesterID:  r.RequesterID,
		ContractorID: r.ContractorID,
		Contractor:   contractorName(q, r.ContractorID),
		Area:         r.Area,
		ValidFrom:    r.ValidFrom,
		ValidTo:      r.ValidTo,
		Capabilities: r.Capabilities,
		Purpose:      r.Purpose,
		CreatedAt:    r.CreatedAt,
		Aircraft:     []assignView{},
	}
	ras, err := s.requestAircraft(q, r.ID)
	if err == nil {
		for _, ra := range ras {
			av := assignView{AircraftID: ra.AircraftID, PilotID: ra.PilotID, PilotName: userName(q, ra.PilotID)}
			if a, err := s.loadAircraft(q, ra.AircraftID); err == nil {
				av.Callsign = a.Callsign
			}
			v.Aircraft = append(v.Aircraft, av)
		}
	}
	return v
}

func (s *Server) buildVersionViews(q dbtx, vs []store.Version) []versionView {
	out := make([]versionView, 0, len(vs))
	for _, v := range vs {
		out = append(out, versionView{
			ID:             v.ID,
			Version:        v.Version,
			Decision:       v.Decision,
			DecisionSource: v.DecisionSource,
			Reason:         v.Reason,
			Polygon:        v.Polygon,
			ValidFrom:      v.ValidFrom,
			ValidTo:        v.ValidTo,
			DecidedBy:      v.DecidedBy,
			DecidedByName:  userName(q, v.DecidedBy),
			CreatedAt:      v.CreatedAt,
		})
	}
	return out
}

// buildCredentialViews masks the single-use token unless the caller is
// ATC or the pilot holding the credential.
func (s *Server) buildCredentialViews(q dbtx, u *store.User, creds []store.Credential) []credentialView {
	out := make([]credentialView, 0, len(creds))
	for _, cr := range creds {
		cv := credentialView{
			ID:           cr.ID,
			VersionID:    cr.VersionID,
			AircraftID:   cr.AircraftID,
			PilotID:      cr.PilotID,
			PilotName:    userName(q, cr.PilotID),
			Status:       cr.Status,
			IssuedAt:     cr.IssuedAt,
			UsedAt:       cr.UsedAt,
			FrozenAt:     cr.FrozenAt,
			FrozenReason: cr.FrozenReason,
		}
		if a, err := s.loadAircraft(q, cr.AircraftID); err == nil {
			cv.Callsign = a.Callsign
		}
		if u.Role == store.RoleATC || u.ID == cr.PilotID {
			cv.Token = cr.Token
		}
		out = append(out, cv)
	}
	return out
}

func (s *Server) pendingAcks(q dbtx, requestID int64) ([]pendingAckView, error) {
	rows, err := q.Query(`SELECT n.id, n.user_id, u.name, n.kind, n.created_at
		FROM notifications n JOIN users u ON u.id = n.user_id
		WHERE n.request_id = ? AND n.acked_at IS NULL
		ORDER BY n.id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pendingAckView{}
	for rows.Next() {
		var p pendingAckView
		var ca string
		if err := rows.Scan(&p.NotificationID, &p.UserID, &p.UserName, &p.Kind, &ca); err != nil {
			return nil, err
		}
		p.CreatedAt = store.ParseTime(ca)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) loadSegments(q dbtx, where string, args ...any) ([]segmentView, error) {
	rows, err := q.Query(`SELECT g.id, g.credential_id, g.request_id, g.aircraft_id, a.callsign,
		g.pilot_id, u.name, g.started_at, g.ended_at, g.deviated, g.deviation_reason, g.deviation_action, g.reported_at
		FROM segments g
		JOIN aircraft a ON a.id = g.aircraft_id
		JOIN users u ON u.id = g.pilot_id
		WHERE `+where+` ORDER BY g.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []segmentView{}
	for rows.Next() {
		var sv segmentView
		var sa, ra string
		var ea sql.NullString
		var dev int
		if err := rows.Scan(&sv.ID, &sv.CredentialID, &sv.RequestID, &sv.AircraftID, &sv.Callsign,
			&sv.PilotID, &sv.PilotName, &sa, &ea, &dev, &sv.DeviationReason, &sv.DeviationAction, &ra); err != nil {
			return nil, err
		}
		sv.StartedAt = store.ParseTime(sa)
		sv.EndedAt = scanNullTime(ea)
		sv.Deviated = dev == 1
		sv.ReportedAt = store.ParseTime(ra)
		out = append(out, sv)
	}
	return out, rows.Err()
}

// requestDetail assembles the full authorization picture: the single
// valid version, spatial conflict locations, credential usage and the
// people who still owe a receipt.
func (s *Server) requestDetail(c echo.Context, u *store.User, r *store.Request) error {
	versions, err := s.loadVersions(s.db, r.ID)
	if err != nil {
		return err
	}
	creds, err := s.loadCredentials(s.db, r.ID)
	if err != nil {
		return err
	}
	pending, err := s.pendingAcks(s.db, r.ID)
	if err != nil {
		return err
	}
	segments, err := s.loadSegments(s.db, `g.request_id = ?`, r.ID)
	if err != nil {
		return err
	}

	resp := map[string]any{
		"request":      s.buildRequestView(s.db, r),
		"versions":     s.buildVersionViews(s.db, versions),
		"credentials":  s.buildCredentialViews(s.db, u, creds),
		"pending_acks": pending,
		"segments":     segments,
	}

	if len(versions) > 0 {
		cur := versions[len(versions)-1]
		curView := s.buildVersionViews(s.db, []store.Version{cur})[0]
		resp["current_version"] = curView
		if cur.Decision != store.DecisionRevoked {
			conflicts, err := s.conflictsFor(s.db, cur.Polygon, r.ID)
			if err != nil {
				return err
			}
			resp["conflicts"] = conflicts
		} else {
			resp["conflicts"] = []Conflict{}
		}
	} else {
		resp["current_version"] = nil
		// Not yet decided: show where the requested area would clash, so
		// ATC sees the helicopter-route crossing before deciding.
		pre, err := s.conflictsFor(s.db, r.Area, r.ID)
		if err != nil {
			return err
		}
		resp["requested_area_conflicts"] = pre
	}
	return c.JSON(http.StatusOK, resp)
}

// ---------- request handlers ----------

type createRequestIn struct {
	Area         geo.Polygon     `json:"area"`
	ValidFrom    string          `json:"valid_from"`
	ValidTo      string          `json:"valid_to"`
	Capabilities json.RawMessage `json:"capabilities"`
	Purpose      string          `json:"purpose"`
	Aircraft     []struct {
		AircraftID int64 `json:"aircraft_id"`
		PilotID    int64 `json:"pilot_id"`
	} `json:"aircraft"`
}

// createRequest: coordinator files a mission by area, time window and
// aircraft capability, assigning aircraft and pilots of the own unit.
func (s *Server) createRequest(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleCoordinator) {
		return c.JSON(http.StatusForbidden, errBody("只有协调员可以提交申请"))
	}
	var in createRequestIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	if !in.Area.Valid() {
		return c.JSON(http.StatusUnprocessableEntity, errBody("任务区域多边形至少需要 3 个顶点"))
	}
	vf, err := parseTimeParam(in.ValidFrom)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("valid_from 需为 RFC3339 时间"))
	}
	vt, err := parseTimeParam(in.ValidTo)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("valid_to 需为 RFC3339 时间"))
	}
	if !vf.Before(vt) {
		return c.JSON(http.StatusUnprocessableEntity, errBody("valid_from 必须早于 valid_to"))
	}
	if len(in.Aircraft) == 0 {
		return c.JSON(http.StatusUnprocessableEntity, errBody("至少指派一架飞行器"))
	}
	caps := in.Capabilities
	if len(caps) == 0 {
		caps = json.RawMessage(`{}`)
	}

	// Contractor isolation: every assigned aircraft and pilot must belong
	// to the coordinator's own contractor.
	for _, as := range in.Aircraft {
		a, err := s.loadAircraft(s.db, as.AircraftID)
		if err != nil {
			return c.JSON(http.StatusUnprocessableEntity, errBody("飞行器不存在"))
		}
		if u.ContractorID == nil || a.ContractorID != *u.ContractorID {
			return c.JSON(http.StatusForbidden, errBody("只能指派本单位的飞行器"))
		}
		row := s.db.QueryRow(`SELECT role, contractor_id FROM users WHERE id = ?`, as.PilotID)
		var role string
		var cid sql.NullInt64
		if err := row.Scan(&role, &cid); err != nil || role != store.RolePilot {
			return c.JSON(http.StatusUnprocessableEntity, errBody("指派的飞手不存在"))
		}
		if !cid.Valid || cid.Int64 != a.ContractorID {
			return c.JSON(http.StatusForbidden, errBody("只能指派本单位的飞手"))
		}
	}

	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO requests(requester_id, contractor_id, area, valid_from, valid_to, capabilities, purpose, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, *u.ContractorID, store.MarshalPolygon(in.Area), store.FmtTime(vf), store.FmtTime(vt), string(caps), in.Purpose, store.FmtTime(now))
	if err != nil {
		return err
	}
	reqID, _ := res.LastInsertId()
	for _, as := range in.Aircraft {
		if _, err := tx.Exec(`INSERT INTO request_aircraft(request_id, aircraft_id, pilot_id) VALUES (?,?,?)`,
			reqID, as.AircraftID, as.PilotID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	r, err := s.loadRequest(s.db, reqID)
	if err != nil {
		return err
	}
	return s.requestDetail(c, u, r)
}

// listRequests: ATC sees all, coordinators their contractor's, pilots
// only the ones they are assigned to. Command uses /api/overview.
func (s *Server) listRequests(c echo.Context) error {
	u := currentUser(c)
	var rows *sql.Rows
	var err error
	switch u.Role {
	case store.RoleATC:
		rows, err = s.db.Query(`SELECT id FROM requests ORDER BY id DESC`)
	case store.RoleCoordinator:
		rows, err = s.db.Query(`SELECT id FROM requests WHERE contractor_id = ? ORDER BY id DESC`, nullInt64(u.ContractorID))
	case store.RolePilot:
		rows, err = s.db.Query(`SELECT DISTINCT r.id FROM requests r
			JOIN request_aircraft ra ON ra.request_id = r.id
			WHERE ra.pilot_id = ? ORDER BY r.id DESC`, u.ID)
	default:
		return c.JSON(http.StatusForbidden, errBody("请使用 /api/overview 查看概况"))
	}
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()

	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		r, err := s.loadRequest(s.db, id)
		if err != nil {
			return err
		}
		item := map[string]any{"request": s.buildRequestView(s.db, r)}
		if cur, err := s.latestVersion(s.db, id); err == nil && cur != nil {
			item["current_version"] = s.buildVersionViews(s.db, []store.Version{*cur})[0]
		} else {
			item["current_version"] = nil
		}
		out = append(out, item)
	}
	return c.JSON(http.StatusOK, map[string]any{"requests": out})
}

// getRequest returns the authorization detail for one request.
func (s *Server) getRequest(c echo.Context) error {
	u := currentUser(c)
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的请求编号"))
	}
	r, err := s.loadRequest(s.db, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, errBody("申请不存在"))
	}
	if !s.canViewRequest(u, r) {
		// 404 rather than 403: do not leak existence across contractors.
		return c.JSON(http.StatusNotFound, errBody("申请不存在"))
	}
	return s.requestDetail(c, u, r)
}

type decideIn struct {
	Action    string       `json:"action"` // approve | shrink | revoke
	Polygon   *geo.Polygon `json:"polygon,omitempty"`
	ValidFrom string       `json:"valid_from,omitempty"`
	ValidTo   string       `json:"valid_to,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Source    string       `json:"source,omitempty"` // manual | rule:avoidance
}

// decide: ATC approves, shrinks or revokes. Every call appends a new
// immutable version; credentials of superseded versions are revoked and
// fresh single-use credentials are issued for the new version.
func (s *Server) decide(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有空管员可以作出授权决定"))
	}
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的请求编号"))
	}
	var in decideIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	if in.Action != "approve" && in.Action != "shrink" && in.Action != "revoke" {
		return c.JSON(http.StatusBadRequest, errBody("action 必须为 approve|shrink|revoke"))
	}
	decision := map[string]string{
		"approve": store.DecisionApproved,
		"shrink":  store.DecisionShrunk,
		"revoke":  store.DecisionRevoked,
	}[in.Action]
	source := in.Source
	if source == "" {
		source = "manual"
	}
	if source != "manual" && source != "rule:avoidance" {
		return c.JSON(http.StatusBadRequest, errBody("source 必须为 manual 或 rule:avoidance"))
	}

	r, err := s.loadRequest(s.db, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, errBody("申请不存在"))
	}
	prev, err := s.latestVersion(s.db, id)
	if err != nil {
		return err
	}

	// Determine the new version's polygon and window.
	poly := r.Area
	vf, vt := r.ValidFrom, r.ValidTo
	if prev != nil {
		poly, vf, vt = prev.Polygon, prev.ValidFrom, prev.ValidTo
	}
	switch decision {
	case store.DecisionApproved:
		if prev == nil {
			poly = r.Area // approve as requested
		}
	case store.DecisionShrunk:
		if in.Polygon == nil || !in.Polygon.Valid() {
			return c.JSON(http.StatusUnprocessableEntity, errBody("缩小范围必须给出有效多边形"))
		}
		base := r.Area
		if prev != nil && prev.Decision != store.DecisionRevoked {
			base = prev.Polygon
		}
		if !geo.Contains(base, *in.Polygon) {
			return c.JSON(http.StatusUnprocessableEntity, errBody("缩小后的范围必须包含于上一版范围"))
		}
		poly = *in.Polygon
	case store.DecisionRevoked:
		// keep previous/requested polygon for the record
	}
	if in.ValidFrom != "" {
		if t, err := parseTimeParam(in.ValidFrom); err == nil {
			vf = t
		} else {
			return c.JSON(http.StatusBadRequest, errBody("valid_from 需为 RFC3339 时间"))
		}
	}
	if in.ValidTo != "" {
		if t, err := parseTimeParam(in.ValidTo); err == nil {
			vt = t
		} else {
			return c.JSON(http.StatusBadRequest, errBody("valid_to 需为 RFC3339 时间"))
		}
	}
	if !vf.Before(vt) {
		return c.JSON(http.StatusUnprocessableEntity, errBody("valid_from 必须早于 valid_to"))
	}

	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nextVer := 1
	if prev != nil {
		nextVer = prev.Version + 1
	}
	res, err := tx.Exec(`INSERT INTO auth_versions(request_id, version, polygon, valid_from, valid_to,
		decision, decided_by, decision_source, reason, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, nextVer, store.MarshalPolygon(poly), store.FmtTime(vf), store.FmtTime(vt),
		decision, u.ID, source, in.Reason, store.FmtTime(now))
	if err != nil {
		return err
	}
	versionID, _ := res.LastInsertId()

	// Supersede or revoke every still-usable credential of this request.
	oldCreds, err := s.loadCredentials(tx, id)
	if err != nil {
		return err
	}
	for _, cr := range oldCreds {
		if cr.Status != store.CredActive && cr.Status != store.CredFrozen {
			continue
		}
		note := "superseded"
		kind := store.NotifRevoked
		msg := "授权已更新版本，原凭证作废"
		if decision == store.DecisionRevoked {
			note = "revoked"
			msg = "授权已被空管撤销，凭证作废"
		}
		if _, err := tx.Exec(`UPDATE credentials SET status='revoked', frozen_reason=? WHERE id=?`, note, cr.ID); err != nil {
			return err
		}
		credID := cr.ID
		if err := notify(tx, cr.PilotID, &id, &credID, kind, msg, now); err != nil {
			return err
		}
	}

	// Issue fresh single-use credentials for the new version.
	if decision != store.DecisionRevoked {
		ras, err := s.requestAircraft(tx, id)
		if err != nil {
			return err
		}
		for _, ra := range ras {
			if _, err := tx.Exec(`INSERT INTO credentials(version_id, request_id, aircraft_id, pilot_id, token, status, issued_at)
				VALUES (?,?,?,?,?,'active',?)`, versionID, id, ra.AircraftID, ra.PilotID, newToken(), store.FmtTime(now)); err != nil {
				return err
			}
			msg := "新许可已签发（第 " + strconv.Itoa(nextVer) + " 版），凭证可用"
			if decision == store.DecisionShrunk {
				msg = "许可范围已缩小（第 " + strconv.Itoa(nextVer) + " 版），新凭证已签发"
			}
			if err := notify(tx, ra.PilotID, &id, nil, store.NotifDecision, msg, now); err != nil {
				return err
			}
		}
	}

	// The new polygon may itself clash with a corridor or another
	// authorization; freeze immediately if so.
	if err := s.evaluateFreezes(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.requestDetail(c, u, r)
}
