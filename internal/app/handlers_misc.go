package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"airspace/internal/geo"
	"airspace/internal/store"

	"github.com/labstack/echo/v4"
)

// ---------- rescue routes ----------

type routeView struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Corridor geo.Polygon `json:"corridor"`
	Active   bool        `json:"active"`
	Version  int         `json:"version"`
	Updated  time.Time   `json:"updated_at"`
}

func toRouteView(r store.RescueRoute) routeView {
	return routeView{ID: r.ID, Name: r.Name, Corridor: r.Corridor, Active: r.Active, Version: r.Version, Updated: r.UpdatedAt}
}

func (s *Server) listRoutes(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC, store.RoleCommand, store.RoleCoordinator) {
		return c.JSON(http.StatusForbidden, errBody("无权查看救援航线"))
	}
	rows, err := s.db.Query(`SELECT id, name, corridor, active, version, updated_by, created_at, updated_at FROM rescue_routes ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []routeView{}
	for rows.Next() {
		var r store.RescueRoute
		var corridor, ca, ua string
		var active int
		var ub sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Name, &corridor, &active, &r.Version, &ub, &ca, &ua); err != nil {
			return err
		}
		r.Corridor = store.UnmarshalPolygon(corridor)
		r.Active = active == 1
		r.UpdatedAt = store.ParseTime(ua)
		out = append(out, toRouteView(r))
	}
	return c.JSON(http.StatusOK, map[string]any{"routes": out})
}

type routeIn struct {
	Name     string      `json:"name"`
	Corridor geo.Polygon `json:"corridor"`
}

func (s *Server) createRoute(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有空管员可以维护救援航线"))
	}
	var in routeIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	if in.Name == "" || !in.Corridor.Valid() {
		return c.JSON(http.StatusUnprocessableEntity, errBody("航线名称与走廊多边形必填"))
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO rescue_routes(name, corridor, active, version, updated_by, created_at, updated_at)
		VALUES (?,?,1,1,?,?,?)`, in.Name, store.MarshalPolygon(in.Corridor), u.ID, store.FmtTime(now), store.FmtTime(now))
	if err != nil {
		return err
	}
	// A new corridor can freeze credentials immediately.
	if err := s.evaluateFreezes(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rid, _ := res.LastInsertId()
	return c.JSON(http.StatusOK, map[string]any{"id": rid})
}

type routeUpdateIn struct {
	Corridor *geo.Polygon `json:"corridor,omitempty"`
	Active   *bool        `json:"active,omitempty"`
}

// updateRoute: a rescue-route update is one of the freeze triggers.
func (s *Server) updateRoute(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有空管员可以维护救援航线"))
	}
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的航线编号"))
	}
	var in routeUpdateIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	if in.Corridor != nil && !in.Corridor.Valid() {
		return c.JSON(http.StatusUnprocessableEntity, errBody("走廊多边形至少需要 3 个顶点"))
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if in.Corridor != nil {
		if _, err := tx.Exec(`UPDATE rescue_routes SET corridor=?, version=version+1, updated_by=?, updated_at=? WHERE id=?`,
			store.MarshalPolygon(*in.Corridor), u.ID, store.FmtTime(now), id); err != nil {
			return err
		}
	}
	if in.Active != nil {
		v := 0
		if *in.Active {
			v = 1
		}
		if _, err := tx.Exec(`UPDATE rescue_routes SET active=?, version=version+1, updated_by=?, updated_at=? WHERE id=?`,
			v, u.ID, store.FmtTime(now), id); err != nil {
			return err
		}
	}
	if err := s.evaluateFreezes(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

// ---------- aircraft ----------

func (s *Server) listAircraft(c echo.Context) error {
	u := currentUser(c)
	var rows *sql.Rows
	var err error
	switch u.Role {
	case store.RoleATC, store.RoleCommand:
		rows, err = s.db.Query(`SELECT id, callsign, contractor_id, capabilities, link_status, updated_at FROM aircraft ORDER BY id`)
	case store.RoleCoordinator, store.RolePilot:
		rows, err = s.db.Query(`SELECT id, callsign, contractor_id, capabilities, link_status, updated_at FROM aircraft WHERE contractor_id = ? ORDER BY id`, nullInt64(u.ContractorID))
	default:
		return c.JSON(http.StatusForbidden, errBody("无权查看飞行器"))
	}
	if err != nil {
		return err
	}
	var aircraft []store.Aircraft
	var capsList []string
	for rows.Next() {
		var a store.Aircraft
		var caps, ua string
		if err := rows.Scan(&a.ID, &a.Callsign, &a.ContractorID, &caps, &a.LinkStatus, &ua); err != nil {
			rows.Close()
			return err
		}
		a.UpdatedAt = store.ParseTime(ua)
		aircraft = append(aircraft, a)
		capsList = append(capsList, caps)
	}
	rows.Close()
	out := []map[string]any{}
	for i, a := range aircraft {
		out = append(out, map[string]any{
			"id": a.ID, "callsign": a.Callsign, "contractor_id": a.ContractorID,
			"contractor":   contractorName(s.db, a.ContractorID),
			"capabilities": json.RawMessage(capsList[i]), "link_status": a.LinkStatus, "updated_at": a.UpdatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"aircraft": out})
}

type linkIn struct {
	Status string `json:"status"` // ok | lost
}

// setLink: telemetry reports an aircraft as lost/recovered. Losing the
// link freezes its active credentials in the same transaction.
func (s *Server) setLink(c echo.Context) error {
	u := currentUser(c)
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的飞行器编号"))
	}
	var in linkIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	if in.Status != "ok" && in.Status != "lost" {
		return c.JSON(http.StatusBadRequest, errBody("status 必须为 ok|lost"))
	}
	a, err := s.loadAircraft(s.db, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, errBody("飞行器不存在"))
	}
	if u.Role == store.RoleCoordinator {
		if u.ContractorID == nil || a.ContractorID != *u.ContractorID {
			return c.JSON(http.StatusForbidden, errBody("只能上报本单位飞行器的链路状态"))
		}
	} else if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("无权上报链路状态"))
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE aircraft SET link_status=?, updated_at=? WHERE id=?`, in.Status, store.FmtTime(now), id); err != nil {
		return err
	}
	if in.Status == "lost" {
		if err := s.evaluateFreezes(tx, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"status": in.Status})
}

// ---------- credentials ----------

// myCredentials: a pilot sees only their own credentials.
func (s *Server) myCredentials(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RolePilot) {
		return c.JSON(http.StatusForbidden, errBody("只有飞手持有凭证"))
	}
	rows, err := s.db.Query(`SELECT id, version_id, request_id, aircraft_id, pilot_id, token, status,
		issued_at, used_at, frozen_at, frozen_reason
		FROM credentials WHERE pilot_id = ? ORDER BY id DESC`, u.ID)
	if err != nil {
		return err
	}
	creds, err := scanCredentials(rows)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"credentials": s.buildCredentialViews(s.db, u, creds)})
}

type useIn struct {
	Token string `json:"token"`
}

// useCredential burns a single-use credential at takeoff. The state
// transition is atomic: a second use — replayed, delayed or concurrent —
// is rejected with the current state.
func (s *Server) useCredential(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RolePilot) {
		return c.JSON(http.StatusForbidden, errBody("只有飞手可以使用凭证"))
	}
	var in useIn
	if err := c.Bind(&in); err != nil || in.Token == "" {
		return c.JSON(http.StatusBadRequest, errBody("缺少凭证 token"))
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT c.id, c.pilot_id, c.status, c.frozen_reason, v.valid_from, v.valid_to
		FROM credentials c JOIN auth_versions v ON v.id = c.version_id WHERE c.token = ?`, in.Token)
	var credID, pilotID int64
	var status, frozenReason, vf, vt string
	if err := row.Scan(&credID, &pilotID, &status, &frozenReason, &vf, &vt); err != nil {
		return c.JSON(http.StatusNotFound, errBody("凭证不存在"))
	}
	if pilotID != u.ID {
		return c.JSON(http.StatusForbidden, errBody("凭证不属于当前飞手"))
	}
	switch status {
	case store.CredConsumed:
		return c.JSON(http.StatusConflict, errBody("凭证已使用，仅能起飞一次"))
	case store.CredFrozen:
		return c.JSON(http.StatusConflict, errBody("凭证已冻结："+frozenReason))
	case store.CredRevoked:
		return c.JSON(http.StatusConflict, errBody("凭证已撤销"))
	}
	if now.Before(store.ParseTime(vf)) || now.After(store.ParseTime(vt)) {
		return c.JSON(http.StatusConflict, errBody("当前不在许可有效时段内"))
	}
	res, err := tx.Exec(`UPDATE credentials SET status='consumed', used_at=? WHERE id=? AND status='active'`,
		store.FmtTime(now), credID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return c.JSON(http.StatusConflict, errBody("凭证状态已变化，请刷新后重试"))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"id": credID, "status": store.CredConsumed, "used_at": now})
}

// unfreeze: ATC explicitly returns a frozen credential to service. The
// system never unfreezes on its own.
func (s *Server) unfreeze(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有空管员可以解除冻结"))
	}
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的凭证编号"))
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT request_id, pilot_id, status FROM credentials WHERE id = ?`, id)
	var reqID, pilotID int64
	var status string
	if err := row.Scan(&reqID, &pilotID, &status); err != nil {
		return c.JSON(http.StatusNotFound, errBody("凭证不存在"))
	}
	if status != store.CredFrozen {
		return c.JSON(http.StatusConflict, errBody("凭证当前不在冻结状态"))
	}
	if _, err := tx.Exec(`UPDATE credentials SET status='active', frozen_at=NULL, frozen_reason='' WHERE id=?`, id); err != nil {
		return err
	}
	credID := id
	if err := notify(tx, pilotID, &reqID, &credID, store.NotifUnfrozen, "凭证已由空管解除冻结，恢复可用", now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"status": store.CredActive})
}

// ---------- notifications ----------

func (s *Server) myNotifications(c echo.Context) error {
	u := currentUser(c)
	rows, err := s.db.Query(`SELECT id, user_id, request_id, credential_id, kind, message, created_at, acked_at, ack_receipt
		FROM notifications WHERE user_id = ? ORDER BY id DESC`, u.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var n store.Notification
		var rid, cid sql.NullInt64
		var ca string
		var acked, receipt sql.NullString
		if err := rows.Scan(&n.ID, &n.UserID, &rid, &cid, &n.Kind, &n.Message, &ca, &acked, &receipt); err != nil {
			return err
		}
		item := map[string]any{
			"id": n.ID, "kind": n.Kind, "message": n.Message, "created_at": store.ParseTime(ca),
		}
		if rid.Valid {
			item["request_id"] = rid.Int64
		}
		if cid.Valid {
			item["credential_id"] = cid.Int64
		}
		if acked.Valid {
			item["acked_at"] = store.ParseTime(acked.String)
		}
		if receipt.Valid {
			item["ack_receipt"] = receipt.String
		}
		out = append(out, item)
	}
	return c.JSON(http.StatusOK, map[string]any{"notifications": out})
}

type ackIn struct {
	Receipt string `json:"receipt"` // client-generated idempotency key
}

// ackNotification confirms a notification with a client receipt id.
// Delayed or duplicated deliveries are safe: the first ack wins and any
// replay gets the stored result back.
func (s *Server) ackNotification(c echo.Context) error {
	u := currentUser(c)
	id, err := parseID(c, "id")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("无效的通知编号"))
	}
	var in ackIn
	if err := c.Bind(&in); err != nil || in.Receipt == "" {
		return c.JSON(http.StatusBadRequest, errBody("缺少回执编号 receipt"))
	}
	row := s.db.QueryRow(`SELECT user_id, acked_at, ack_receipt FROM notifications WHERE id = ?`, id)
	var userID int64
	var ackedAt, receipt sql.NullString
	if err := row.Scan(&userID, &ackedAt, &receipt); err != nil {
		return c.JSON(http.StatusNotFound, errBody("通知不存在"))
	}
	if userID != u.ID {
		return c.JSON(http.StatusForbidden, errBody("只能确认发给本人的通知"))
	}
	if ackedAt.Valid {
		// Already confirmed — return the stored state idempotently.
		return c.JSON(http.StatusOK, map[string]any{
			"id": id, "already_acked": true, "acked_at": store.ParseTime(ackedAt.String), "ack_receipt": receipt.String,
		})
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE notifications SET acked_at=?, ack_receipt=? WHERE id=? AND acked_at IS NULL`,
		store.FmtTime(now), in.Receipt, id)
	if err != nil {
		if isUniqueViolation(err) {
			return c.JSON(http.StatusConflict, errBody("该回执编号已用于其他通知"))
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost a race with a concurrent ack; read back the stored state.
		row := s.db.QueryRow(`SELECT acked_at, ack_receipt FROM notifications WHERE id = ?`, id)
		var acked, rcpt sql.NullString
		if err := row.Scan(&acked, &rcpt); err == nil && acked.Valid {
			return c.JSON(http.StatusOK, map[string]any{
				"id": id, "already_acked": true, "acked_at": store.ParseTime(acked.String), "ack_receipt": rcpt.String,
			})
		}
		return c.JSON(http.StatusConflict, errBody("回执冲突，请重试"))
	}
	return c.JSON(http.StatusOK, map[string]any{"id": id, "acked": true, "acked_at": now, "ack_receipt": in.Receipt})
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------- segments ----------

type segmentIn struct {
	CredentialID    int64  `json:"credential_id"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	Deviated        bool   `json:"deviated"`
	DeviationReason string `json:"deviation_reason,omitempty"`
	DeviationAction string `json:"deviation_action,omitempty"`
}

// reportSegment registers a flown leg. Segments are insert-only: there
// is deliberately no update or delete endpoint, so a flown leg can never
// be rewritten as unexecuted. Deviations must be reported truthfully
// with their handling.
func (s *Server) reportSegment(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RolePilot) {
		return c.JSON(http.StatusForbidden, errBody("只有飞手可以登记航段"))
	}
	var in segmentIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, errBody("请求体无法解析"))
	}
	start, err := parseTimeParam(in.StartedAt)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errBody("started_at 需为 RFC3339 时间"))
	}
	var end *time.Time
	if in.EndedAt != "" {
		t, err := parseTimeParam(in.EndedAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errBody("ended_at 需为 RFC3339 时间"))
		}
		end = &t
	}
	if in.Deviated && (in.DeviationReason == "" || in.DeviationAction == "") {
		return c.JSON(http.StatusUnprocessableEntity, errBody("偏航必须如实登记原因与处置措施"))
	}
	row := s.db.QueryRow(`SELECT request_id, aircraft_id, pilot_id, status FROM credentials WHERE id = ?`, in.CredentialID)
	var reqID, aircraftID, pilotID int64
	var status string
	if err := row.Scan(&reqID, &aircraftID, &pilotID, &status); err != nil {
		return c.JSON(http.StatusNotFound, errBody("凭证不存在"))
	}
	if pilotID != u.ID {
		return c.JSON(http.StatusForbidden, errBody("只能登记本人凭证的航段"))
	}
	if status != store.CredConsumed {
		return c.JSON(http.StatusConflict, errBody("只有已起飞（已使用）凭证的航段可以登记"))
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO segments(credential_id, request_id, aircraft_id, pilot_id,
		started_at, ended_at, deviated, deviation_reason, deviation_action, reported_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		in.CredentialID, reqID, aircraftID, u.ID, store.FmtTime(start), nullTimeStr(end),
		boolToInt(in.Deviated), in.DeviationReason, in.DeviationAction, store.FmtTime(now))
	if err != nil {
		return err
	}
	segID, _ := res.LastInsertId()
	return c.JSON(http.StatusOK, map[string]any{"id": segID, "reported_at": now})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) mySegments(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RolePilot) {
		return c.JSON(http.StatusForbidden, errBody("只有飞手可以查看本人航段"))
	}
	segs, err := s.loadSegments(s.db, `g.pilot_id = ?`, u.ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"segments": segs})
}

func (s *Server) listSegments(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有空管员可以查看全部航段"))
	}
	segs, err := s.loadSegments(s.db, `1=1`)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"segments": segs})
}

// ---------- command overview ----------

// overview: the rescue command's conflict picture — live clashes,
// credential usage counts and the people who still owe a receipt.
// Contractor-internal details (tokens, notification bodies) stay hidden.
func (s *Server) overview(c echo.Context) error {
	u := currentUser(c)
	if !hasRole(u, store.RoleCommand, store.RoleATC) {
		return c.JSON(http.StatusForbidden, errBody("只有救援指挥或空管员可以查看概况"))
	}
	conflicts, err := s.allConflicts(s.db)
	if err != nil {
		return err
	}

	credStats := map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM credentials GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return err
		}
		credStats[st] = n
	}
	rows.Close()

	type pendingPerson struct {
		UserID     int64     `json:"user_id"`
		Name       string    `json:"name"`
		Role       string    `json:"role"`
		Contractor string    `json:"contractor,omitempty"`
		Pending    int       `json:"pending"`
		Oldest     time.Time `json:"oldest"`
	}
	prows, err := s.db.Query(`SELECT u.id, u.name, u.role, IFNULL(ct.name,''), COUNT(*), MIN(n.created_at)
		FROM notifications n
		JOIN users u ON u.id = n.user_id
		LEFT JOIN contractors ct ON ct.id = u.contractor_id
		WHERE n.acked_at IS NULL
		GROUP BY u.id ORDER BY MIN(n.created_at)`)
	if err != nil {
		return err
	}
	people := []pendingPerson{}
	for prows.Next() {
		var p pendingPerson
		var oldest string
		if err := prows.Scan(&p.UserID, &p.Name, &p.Role, &p.Contractor, &p.Pending, &oldest); err != nil {
			prows.Close()
			return err
		}
		p.Oldest = store.ParseTime(oldest)
		people = append(people, p)
	}
	prows.Close()

	return c.JSON(http.StatusOK, map[string]any{
		"conflicts":            conflicts,
		"credentials_by_state": credStats,
		"pending_people":       people,
	})
}
