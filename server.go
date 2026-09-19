package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

type Server struct {
	store *Store
}

// authed loads the API key and attaches the user to the context.
func (s *Server) authed(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		key := c.Request().Header.Get("X-API-Key")
		if key == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "missing X-API-Key")
		}
		u, err := s.store.userByKey(key)
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid API key")
		}
		c.Set("user", u)
		return next(c)
	}
}

func currentUser(c echo.Context) *userRow { return c.Get("user").(*userRow) }

func writeError(c echo.Context, err error) error {
	if ae, ok := err.(*apiError); ok {
		// ATC conflict refusals carry a structured suggestion body.
		if ae.Status == http.StatusUnprocessableEntity && json.Valid([]byte(ae.Msg)) {
			return c.Blob(ae.Status, echo.MIMEApplicationJSON, []byte(ae.Msg))
		}
		return c.JSON(ae.Status, map[string]string{"error": ae.Msg})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// idempotent wraps a mutating handler: a repeated field receipt (network
// backlog or client retry) with the same Idempotency-Key replays the stored
// unique effective response instead of executing twice.
func (s *Server) idempotent(scope string, fn func(c echo.Context, u *userRow, body []byte) (int, any, error)) echo.HandlerFunc {
	return s.authed(func(c echo.Context) error {
		u := currentUser(c)
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return writeError(c, fail(400, "cannot read body"))
		}
		key := c.Request().Header.Get("Idempotency-Key")
		if key == "" {
			return writeError(c, fail(400, "Idempotency-Key header is required for mutating requests"))
		}
		sum := sha256.Sum256(body)
		reqHash := hex.EncodeToString(sum[:])
		replay, err := s.store.idemLookup(key, u.APIKey, scope, reqHash)
		if err != nil {
			return writeError(c, err)
		}
		if replay != nil {
			c.Response().Header().Set("Idempotent-Replayed", "true")
			return c.Blob(replay.StatusCode, echo.MIMEApplicationJSON, replay.Body)
		}
		status, result, gerr := fn(c, u, body)
		if gerr != nil {
			return writeError(c, gerr)
		}
		out, _ := json.Marshal(result)
		s.store.idemStore(key, u.APIKey, scope, reqHash, status, out)
		c.Response().Header().Set("Idempotent-Replayed", "false")
		return c.Blob(status, echo.MIMEApplicationJSON, out)
	})
}

func decodeBody(body []byte, dst any) error {
	if len(body) == 0 {
		return fail(400, "request body is required")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fail(400, "invalid JSON: "+err.Error())
	}
	return nil
}

// ---------- handlers ----------

func (s *Server) createPermit(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto createPermitDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	id, err := s.store.createPermit(u, dto)
	if err != nil {
		return 0, nil, err
	}
	detail, err := s.store.permitDetail(id, u)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, detail, nil
}

func (s *Server) listPermits(c echo.Context) error {
	u := currentUser(c)
	list, err := s.store.listPermits(u)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) getPermit(c echo.Context) error {
	u := currentUser(c)
	detail, err := s.store.permitDetail(c.Param("id"), u)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(http.StatusOK, detail)
}

func (s *Server) decide(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto decisionDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	res, err := s.store.decide(u, c.Param("id"), dto)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, res, nil
}

type aircraftDTO struct {
	Callsign     string   `json:"callsign"`
	Capabilities []string `json:"capabilities"`
}

func (s *Server) registerAircraft(c echo.Context, u *userRow, body []byte) (int, any, error) {
	if u.Role != "coordinator" {
		return 0, nil, fail(403, "only coordinators register aircraft")
	}
	var dto aircraftDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	if dto.Callsign == "" {
		return 0, nil, fail(400, "callsign required")
	}
	id := newID("uav")
	caps, _ := json.Marshal(dto.Capabilities)
	if _, err := s.store.db.Exec(
		`INSERT INTO aircraft(id, contractor_id, callsign, capabilities) VALUES(?,?,?,?)`,
		id, u.ContractorID, dto.Callsign, string(caps)); err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{
		"id": id, "contractorId": u.ContractorID,
		"callsign": dto.Callsign, "capabilities": dto.Capabilities,
	}, nil
}

func (s *Server) listAircraft(c echo.Context) error {
	u := currentUser(c)
	q := `SELECT id, contractor_id, callsign, capabilities, link_status, COALESCE(last_seen_at,'')
	      FROM aircraft`
	var args []interface{}
	if u.Role == "coordinator" {
		q += ` WHERE contractor_id=?`
		args = append(args, u.ContractorID)
	} else if u.Role == "pilot" {
		q += ` WHERE id=?`
		args = append(args, u.AircraftID)
	}
	q += ` ORDER BY callsign`
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		return writeError(c, err)
	}
	defer rows.Close()
	out := []aircraftRow{}
	for rows.Next() {
		var a aircraftRow
		var caps string
		if err := rows.Scan(&a.ID, &a.ContractorID, &a.Callsign, &caps, &a.LinkStatus, &a.LastSeenAt); err != nil {
			return writeError(c, err)
		}
		_ = json.Unmarshal([]byte(caps), &a.Capabilities)
		out = append(out, a)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) reportLink(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto heartbeatDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	if err := s.store.reportLink(u, c.Param("id"), dto.Status); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]string{
		"aircraftId": c.Param("id"), "linkStatus": dto.Status,
	}, nil
}

func (s *Server) createRoute(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto routeDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	ref, err := s.store.upsertRoute(u, "", dto)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, ref, nil
}

func (s *Server) updateRoute(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto routeDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	ref, err := s.store.upsertRoute(u, c.Param("id"), dto)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, ref, nil
}

func (s *Server) listRoutes(c echo.Context) error {
	rows, err := s.store.db.Query(
		`SELECT id, name, version, corridor, created_at FROM rescue_routes WHERE active=1 ORDER BY created_at`)
	if err != nil {
		return writeError(c, err)
	}
	defer rows.Close()
	type routeView struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Version  int             `json:"version"`
		Corridor json.RawMessage `json:"corridor"`
	}
	out := []routeView{}
	for rows.Next() {
		var v routeView
		var poly string
		if err := rows.Scan(&v.ID, &v.Name, &v.Version, &poly, new(string)); err != nil {
			return writeError(c, err)
		}
		v.Corridor = json.RawMessage(poly)
		out = append(out, v)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) redeem(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto redeemDTO
	_ = json.Unmarshal(body, &dto) // note optional, body may be empty
	res, err := s.store.redeem(u, c.Param("token"), dto.Note)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, res, nil
}

func (s *Server) reportSegment(c echo.Context, u *userRow, body []byte) (int, any, error) {
	var dto segmentDTO
	if err := decodeBody(body, &dto); err != nil {
		return 0, nil, err
	}
	v, err := s.store.reportSegment(u, c.Param("id"), dto)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, v, nil
}

func (s *Server) listNotifications(c echo.Context) error {
	u := currentUser(c)
	out, err := s.store.listNotifications(u)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) ack(c echo.Context) error {
	u := currentUser(c)
	v, err := s.store.acknowledge(u, c.Param("id"))
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(http.StatusOK, v)
}

func (s *Server) dashboard(c echo.Context) error {
	u := currentUser(c)
	if u.Role != "rescue" && u.Role != "atc" {
		return writeError(c, fail(403, "rescue command dashboard only"))
	}
	return c.JSON(http.StatusOK, s.store.rescueDashboard())
}

func (s *Server) events(c echo.Context) error {
	u := currentUser(c)
	if u.Role != "atc" && u.Role != "rescue" {
		return writeError(c, fail(403, "audit trail is restricted to ATC and rescue command"))
	}
	rows, err := s.store.db.Query(
		`SELECT id, COALESCE(permit_id,''), type, source, COALESCE(actor,''), COALESCE(detail,''), created_at
		 FROM events ORDER BY created_at DESC LIMIT 300`)
	if err != nil {
		return writeError(c, err)
	}
	defer rows.Close()
	type evt struct {
		ID        string `json:"id"`
		PermitID  string `json:"permitId"`
		Type      string `json:"type"`
		Source    string `json:"source"`
		Actor     string `json:"actor"`
		Detail    string `json:"detail"`
		CreatedAt string `json:"createdAt"`
	}
	out := []evt{}
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.ID, &e.PermitID, &e.Type, &e.Source, &e.Actor, &e.Detail, &e.CreatedAt); err != nil {
			return writeError(c, err)
		}
		out = append(out, e)
	}
	return c.JSON(http.StatusOK, out)
}

// ---------- router ----------

func newServer(store *Store) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	s := &Server{store: store}

	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(200, map[string]string{"status": "ok"})
	})

	api := e.Group("/api", s.authed)

	api.GET("/permits", s.listPermits)
	api.POST("/permits", s.idempotent("permit.create", s.createPermit))
	api.GET("/permits/:id", s.getPermit)
	api.POST("/permits/:id/decisions", s.idempotent("permit.decide", s.decide))

	api.GET("/aircraft", s.listAircraft)
	api.POST("/aircraft", s.idempotent("aircraft.register", s.registerAircraft))
	api.POST("/aircraft/:id/link", s.idempotent("aircraft.link", s.reportLink))

	api.GET("/routes", s.listRoutes)
	api.POST("/routes", s.idempotent("route.create", s.createRoute))
	api.PUT("/routes/:id", s.idempotent("route.update", s.updateRoute))

	api.POST("/credentials/:token/redeem", s.idempotent("credential.redeem", s.redeem))
	api.POST("/segments/:id", s.idempotent("segment.report", s.reportSegment))

	api.GET("/notifications", s.listNotifications)
	api.POST("/notifications/:id/ack", s.ack)

	api.GET("/dashboard/rescue", s.dashboard)
	api.GET("/events", s.events)

	return e
}

var _ = sql.ErrNoRows
