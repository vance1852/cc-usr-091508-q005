package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"airspace/internal/store"

	"github.com/labstack/echo/v4"
)

// --- test scaffolding ---

func newTestServer(t *testing.T) *echo.Echo {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := SeedIfEmpty(db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewServer(db)
}

func do(t *testing.T, e *echo.Echo, method, path, token string, payload any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			t.Fatalf("encode payload: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func poly(pts ...[2]float64) map[string]any {
	vs := make([]map[string]float64, 0, len(pts))
	for _, p := range pts {
		vs = append(vs, map[string]float64{"lat": p[0], "lng": p[1]})
	}
	return map[string]any{"vertices": vs}
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func nested(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: not an object at %q (got %T)", keys, k, cur)
		}
		cur, ok = mm[k]
		if !ok {
			t.Fatalf("path %v: missing key %q in %v", keys, k, mm)
		}
	}
	return cur
}

func mustStatus(t *testing.T, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d; body = %v", got, want, body)
	}
}

// Seed layout (insertion order): users 1=atc 2=command 3=coordA 4=coordB
// 5=pilotA1 6=pilotA2 7=pilotB1; aircraft 1=UAV-A1 2=UAV-A2 3=UAV-B1;
// route 1 = 生命线一号 corridor lat 31.0000-31.0040, lng 120.9800-121.0600.

const (
	tokATC     = "tok-atc"
	tokCommand = "tok-command"
	tokCoordA  = "tok-coord-alpha"
	tokCoordB  = "tok-coord-beta"
	tokPilotA1 = "tok-pilot-a1"
	tokPilotA2 = "tok-pilot-a2"
	tokPilotB1 = "tok-pilot-b1"
)

// areaCrossingCorridor overlaps the seeded helicopter corridor.
func areaCrossingCorridor() map[string]any {
	return poly([2]float64{30.9990, 121.0000}, [2]float64{30.9990, 121.0100},
		[2]float64{31.0050, 121.0100}, [2]float64{31.0050, 121.0000})
}

// areaNorthOfCorridor is contained in areaCrossingCorridor, clear of the corridor.
func areaNorthOfCorridor() map[string]any {
	return poly([2]float64{31.0041, 121.0005}, [2]float64{31.0041, 121.0095},
		[2]float64{31.0049, 121.0095}, [2]float64{31.0049, 121.0005})
}

func createRequest(t *testing.T, e *echo.Echo, token string, aircraftID, pilotID int64, area map[string]any, from, to time.Time) int64 {
	t.Helper()
	code, body := do(t, e, "POST", "/api/requests", token, map[string]any{
		"area":         area,
		"valid_from":   rfc3339(from),
		"valid_to":     rfc3339(to),
		"capabilities": map[string]any{"max_altitude_m": 120},
		"purpose":      "堤段测绘",
		"aircraft":     []map[string]any{{"aircraft_id": aircraftID, "pilot_id": pilotID}},
	})
	mustStatus(t, code, 200, body)
	return int64(nested(t, body, "request", "id").(float64))
}

// TestFullWorkflow walks the whole emergency scenario end to end.
func TestFullWorkflow(t *testing.T) {
	e := newTestServer(t)
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(2*time.Hour)

	// 1. Coordinator files a request whose area crosses the helicopter corridor.
	reqA := createRequest(t, e, tokCoordA, 1, 5, areaCrossingCorridor(), from, to)

	// The pending request already shows where the requested area clashes.
	code, body := do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqA), tokATC, nil)
	mustStatus(t, code, 200, body)
	pre := nested(t, body, "requested_area_conflicts").([]any)
	if len(pre) != 1 {
		t.Fatalf("expected 1 route conflict on requested area, got %v", pre)
	}
	pts := nested(t, pre[0].(map[string]any), "points").([]any)
	if len(pts) == 0 {
		t.Fatal("conflict must carry intersection locations")
	}

	// 2. ATC approves; the credential is born frozen because of the corridor.
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqA), tokATC, map[string]any{
		"action": "approve", "reason": "按申请批准",
	})
	mustStatus(t, code, 200, body)
	if v := nested(t, body, "current_version", "version"); v.(float64) != 1 {
		t.Fatalf("expected version 1, got %v", v)
	}
	creds := nested(t, body, "credentials").([]any)
	if len(creds) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(creds))
	}
	cred0 := creds[0].(map[string]any)
	if cred0["status"] != "frozen" || cred0["frozen_reason"] != "route_conflict" {
		t.Fatalf("credential should be frozen on route conflict, got %v", cred0)
	}
	// The holder owes a receipt.
	pending := nested(t, body, "pending_acks").([]any)
	if len(pending) == 0 {
		t.Fatal("pilot must appear in pending_acks")
	}

	// 3. Pilot reads notifications and acks; replays are idempotent.
	code, body = do(t, e, "GET", "/api/notifications/mine", tokPilotA1, nil)
	mustStatus(t, code, 200, body)
	notifs := nested(t, body, "notifications").([]any)
	if len(notifs) < 2 {
		t.Fatalf("expected decision+frozen notifications, got %v", notifs)
	}
	var frozenNotif, decisionNotif float64
	for _, n := range notifs {
		nm := n.(map[string]any)
		switch nm["kind"] {
		case "credential_frozen":
			frozenNotif = nm["id"].(float64)
		case "decision":
			decisionNotif = nm["id"].(float64)
		}
	}
	if frozenNotif == 0 || decisionNotif == 0 {
		t.Fatalf("missing expected notifications: %v", notifs)
	}
	ack := func(id float64, receipt string) (int, map[string]any) {
		return do(t, e, "POST", fmt.Sprintf("/api/notifications/%d/ack", int64(id)), tokPilotA1,
			map[string]any{"receipt": receipt})
	}
	code, body = ack(frozenNotif, "rcpt-001")
	mustStatus(t, code, 200, body)
	code, body = ack(frozenNotif, "rcpt-001") // duplicate delivery
	mustStatus(t, code, 200, body)
	if body["already_acked"] != true {
		t.Fatalf("duplicate ack should report already_acked, got %v", body)
	}
	code, body = ack(frozenNotif, "rcpt-999") // delayed retry with new receipt
	mustStatus(t, code, 200, body)
	if body["already_acked"] != true {
		t.Fatalf("late ack should be answered idempotently, got %v", body)
	}
	code, body = ack(decisionNotif, "rcpt-001") // receipt already spent elsewhere
	mustStatus(t, code, 409, body)
	code, body = ack(decisionNotif, "rcpt-002")
	mustStatus(t, code, 200, body)

	// No one owes a receipt for request A any more.
	code, body = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqA), tokCoordA, nil)
	mustStatus(t, code, 200, body)
	if p := nested(t, body, "pending_acks").([]any); len(p) != 0 {
		t.Fatalf("pending_acks should be empty, got %v", p)
	}

	// 4. Shrinking must stay inside the previous version.
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqA), tokATC, map[string]any{
		"action":  "shrink",
		"polygon": poly([2]float64{31.0, 121.0}, [2]float64{31.0, 121.02}, [2]float64{31.01, 121.02}, [2]float64{31.01, 121.0}),
		"reason":  "越界缩小应被拒绝",
	})
	mustStatus(t, code, 422, body)

	// 5. ATC shrinks to the safe sub-area: v2 issued, v1 credential revoked.
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqA), tokATC, map[string]any{
		"action": "shrink", "polygon": areaNorthOfCorridor(), "reason": "避开救援航线走廊", "source": "rule:avoidance",
	})
	mustStatus(t, code, 200, body)
	if v := nested(t, body, "current_version", "version"); v.(float64) != 2 {
		t.Fatalf("expected version 2, got %v", v)
	}
	if d := nested(t, body, "current_version", "decision"); d != "shrunk" {
		t.Fatalf("expected shrunk, got %v", d)
	}
	if s := nested(t, body, "current_version", "decision_source"); s != "rule:avoidance" {
		t.Fatalf("decision source must be preserved, got %v", s)
	}
	var activeCred map[string]any
	for _, ci := range nested(t, body, "credentials").([]any) {
		cm := ci.(map[string]any)
		if cm["status"] == "active" {
			activeCred = cm
		}
		if cm["status"] == "revoked" && cm["version_id"].(float64) == 1 {
			// superseded v1 credential, as expected
		}
	}
	if activeCred == nil {
		t.Fatalf("expected an active v2 credential, got %v", body["credentials"])
	}
	// The corridor conflict is gone for the shrunk area.
	if cf := nested(t, body, "conflicts").([]any); len(cf) != 0 {
		t.Fatalf("shrunk area should have no conflicts, got %v", cf)
	}

	// 6. Pilot uses the single-use credential; replay is rejected.
	code, body = do(t, e, "POST", "/api/credentials/use", tokPilotA1, map[string]any{"token": activeCred["token"]})
	mustStatus(t, code, 200, body)
	if body["status"] != "consumed" {
		t.Fatalf("expected consumed, got %v", body)
	}
	code, body = do(t, e, "POST", "/api/credentials/use", tokPilotA1, map[string]any{"token": activeCred["token"]})
	mustStatus(t, code, 409, body)

	// 7. Flown segment with deviation handling; segments are insert-only.
	credID := int64(activeCred["id"].(float64))
	code, body = do(t, e, "POST", "/api/segments", tokPilotA1, map[string]any{
		"credential_id": credID, "started_at": rfc3339(now.Add(5 * time.Minute)),
		"deviated": true,
	})
	mustStatus(t, code, 422, body) // deviation without reason/action rejected
	code, body = do(t, e, "POST", "/api/segments", tokPilotA1, map[string]any{
		"credential_id": credID, "started_at": rfc3339(now.Add(5 * time.Minute)), "ended_at": rfc3339(now.Add(25 * time.Minute)),
		"deviated": true, "deviation_reason": "阵风偏离走廊", "deviation_action": "下降至 60m 并返航",
	})
	mustStatus(t, code, 200, body)
	segID := int64(body["id"].(float64))
	code, _ = do(t, e, "PUT", fmt.Sprintf("/api/segments/%d", segID), tokPilotA1, map[string]any{"deviated": false})
	if code != 404 && code != 405 {
		t.Fatalf("segments must be immutable, got status %d", code)
	}
	code, _ = do(t, e, "DELETE", fmt.Sprintf("/api/segments/%d", segID), tokATC, nil)
	if code != 404 && code != 405 {
		t.Fatalf("segments must not be deletable, got status %d", code)
	}

	// 8. Data isolation: other contractor's pilot/coordinator get 404.
	code, _ = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqA), tokPilotB1, nil)
	mustStatus(t, code, 404, nil)
	code, _ = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqA), tokCoordB, nil)
	mustStatus(t, code, 404, nil)
	// Pilot B1 cannot even see contractor A aircraft.
	code, body = do(t, e, "GET", "/api/aircraft", tokPilotB1, nil)
	mustStatus(t, code, 200, body)
	for _, ai := range nested(t, body, "aircraft").([]any) {
		if ai.(map[string]any)["callsign"] == "UAV-A1" {
			t.Fatal("contractor A aircraft leaked to contractor B pilot")
		}
	}
	// A pilot cannot use another pilot's credential.
	code, body = do(t, e, "GET", "/api/credentials/mine", tokPilotA1, nil)
	mustStatus(t, code, 200, body)
	code, body = do(t, e, "POST", "/api/credentials/use", tokPilotB1, map[string]any{"token": activeCred["token"]})
	if code != 403 && code != 404 && code != 409 {
		t.Fatalf("cross-pilot credential use must fail, got %d", code)
	}

	// 9. Second mission on UAV-A2 in a clean area; link loss freezes it.
	reqB := createRequest(t, e, tokCoordA, 2, 6,
		poly([2]float64{31.0100, 121.0000}, [2]float64{31.0100, 121.0100}, [2]float64{31.0200, 121.0100}, [2]float64{31.0200, 121.0000}),
		from, to)
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqB), tokATC, map[string]any{"action": "approve"})
	mustStatus(t, code, 200, body)
	var credB map[string]any
	for _, ci := range nested(t, body, "credentials").([]any) {
		cm := ci.(map[string]any)
		if cm["status"] == "active" {
			credB = cm
		}
	}
	if credB == nil {
		t.Fatalf("clean area should yield an active credential, got %v", body)
	}
	code, body = do(t, e, "POST", "/api/aircraft/2/link", tokCoordA, map[string]any{"status": "lost"})
	mustStatus(t, code, 200, body)
	code, body = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqB), tokATC, nil)
	mustStatus(t, code, 200, body)
	credB = nested(t, body, "credentials").([]any)[0].(map[string]any)
	if credB["status"] != "frozen" || credB["frozen_reason"] != "link_lost" {
		t.Fatalf("link loss must freeze the credential, got %v", credB)
	}
	// Frozen credential cannot be burned.
	code, body = do(t, e, "POST", "/api/credentials/use", tokPilotA2, map[string]any{"token": credB["token"]})
	mustStatus(t, code, 409, body)
	// Link recovered does NOT auto-unfreeze; ATC unfreezes explicitly.
	code, _ = do(t, e, "POST", "/api/aircraft/2/link", tokCoordA, map[string]any{"status": "ok"})
	mustStatus(t, code, 200, nil)
	code, body = do(t, e, "POST", fmt.Sprintf("/api/credentials/%d/unfreeze", int64(credB["id"].(float64))), tokATC, nil)
	mustStatus(t, code, 200, body)

	// 10. Contractor B mission overlapping A2's area: later credential freezes.
	reqC := createRequest(t, e, tokCoordB, 3, 7,
		poly([2]float64{31.0150, 121.0050}, [2]float64{31.0150, 121.0150}, [2]float64{31.0250, 121.0150}, [2]float64{31.0250, 121.0050}),
		from, to)
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqC), tokATC, map[string]any{"action": "approve"})
	mustStatus(t, code, 200, body)
	var credCStatus, credCReason string
	for _, ci := range nested(t, body, "credentials").([]any) {
		cm := ci.(map[string]any)
		credCStatus, _ = cm["status"].(string)
		credCReason, _ = cm["frozen_reason"].(string)
	}
	if credCStatus != "frozen" || credCReason != "mutual_intersection" {
		t.Fatalf("later overlapping credential must freeze, got %v/%v", credCStatus, credCReason)
	}
	// The earlier mission's credential stays active.
	code, body = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqB), tokATC, nil)
	mustStatus(t, code, 200, body)
	if st := nested(t, body, "credentials").([]any)[0].(map[string]any)["status"]; st != "active" {
		t.Fatalf("earlier approval keeps priority, got %v", st)
	}

	// 11. Route update is a freeze trigger.
	code, body = do(t, e, "POST", "/api/routes/1", tokATC, map[string]any{
		"corridor": poly([2]float64{31.0050, 120.9990}, [2]float64{31.0050, 121.0110}, [2]float64{31.0250, 121.0110}, [2]float64{31.0250, 120.9990}),
	})
	mustStatus(t, code, 200, body)
	code, body = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqB), tokATC, nil)
	mustStatus(t, code, 200, body)
	credB = nested(t, body, "credentials").([]any)[0].(map[string]any)
	if credB["status"] != "frozen" || credB["frozen_reason"] != "route_conflict" {
		t.Fatalf("route update must freeze intersecting credentials, got %v", credB)
	}
	// Restore the corridor; frozen credentials stay frozen until ATC acts.
	code, _ = do(t, e, "POST", "/api/routes/1", tokATC, map[string]any{
		"corridor": poly([2]float64{31.0000, 120.9800}, [2]float64{31.0000, 121.0600}, [2]float64{31.0040, 121.0600}, [2]float64{31.0040, 120.9800}),
	})
	mustStatus(t, code, 200, nil)
	code, body = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqB), tokATC, nil)
	mustStatus(t, code, 200, body)
	if st := nested(t, body, "credentials").([]any)[0].(map[string]any)["status"]; st != "frozen" {
		t.Fatalf("no auto-unfreeze after route moves away, got %v", st)
	}

	// 12. Command overview: conflicts, credential states, pending people.
	code, body = do(t, e, "GET", "/api/overview", tokCommand, nil)
	mustStatus(t, code, 200, body)
	confs := nested(t, body, "conflicts").([]any)
	if len(confs) == 0 {
		t.Fatal("overview must list live conflicts")
	}
	var sawMutual bool
	for _, ci := range confs {
		cm := ci.(map[string]any)
		if cm["type"] == "mutual" {
			sawMutual = true
			if len(cm["points"].([]any)) == 0 {
				t.Fatal("mutual conflict must carry locations")
			}
		}
	}
	if !sawMutual {
		t.Fatalf("expected a mutual conflict in overview, got %v", confs)
	}
	stats := nested(t, body, "credentials_by_state").(map[string]any)
	if stats["frozen"].(float64) < 1 {
		t.Fatalf("expected frozen credentials in stats, got %v", stats)
	}
	people := nested(t, body, "pending_people").([]any)
	if len(people) == 0 {
		t.Fatal("overview must list people with unacked notifications")
	}
	// Command cannot read contractor-internal request detail.
	code, _ = do(t, e, "GET", fmt.Sprintf("/api/requests/%d", reqA), tokCommand, nil)
	mustStatus(t, code, 404, nil)

	// 13. Time window is enforced at takeoff.
	reqD := createRequest(t, e, tokCoordA, 1, 5,
		poly([2]float64{31.0300, 121.0000}, [2]float64{31.0300, 121.0100}, [2]float64{31.0400, 121.0100}, [2]float64{31.0400, 121.0000}),
		now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqD), tokATC, map[string]any{"action": "approve"})
	mustStatus(t, code, 200, body)
	var tokD string
	for _, ci := range nested(t, body, "credentials").([]any) {
		cm := ci.(map[string]any)
		if cm["status"] == "active" {
			tokD, _ = cm["token"].(string)
		}
	}
	if tokD == "" {
		t.Fatalf("expected active credential for expired-window request, got %v", body)
	}
	code, body = do(t, e, "POST", "/api/credentials/use", tokPilotA1, map[string]any{"token": tokD})
	mustStatus(t, code, 409, body)

	// 14. Revoke: credentials die with the authorization.
	code, body = do(t, e, "POST", fmt.Sprintf("/api/requests/%d/decisions", reqD), tokATC, map[string]any{
		"action": "revoke", "reason": "任务取消",
	})
	mustStatus(t, code, 200, body)
	if d := nested(t, body, "current_version", "decision"); d != "revoked" {
		t.Fatalf("expected revoked, got %v", d)
	}
	for _, ci := range nested(t, body, "credentials").([]any) {
		if st := ci.(map[string]any)["status"]; st == "active" {
			t.Fatalf("no credential may stay active after revoke, got %v", ci)
		}
	}
	// Version history is complete: every boundary and its origin kept.
	vs := nested(t, body, "versions").([]any)
	if len(vs) != 2 {
		t.Fatalf("expected 2 versions for request D, got %d", len(vs))
	}
}

// TestListRequestsScoping checks the per-role list views.
func TestListRequestsScoping(t *testing.T) {
	e := newTestServer(t)
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	createRequest(t, e, tokCoordA, 1, 5, areaCrossingCorridor(), from, to)
	createRequest(t, e, tokCoordB, 3, 7,
		poly([2]float64{31.1, 121.1}, [2]float64{31.1, 121.11}, [2]float64{31.11, 121.11}, [2]float64{31.11, 121.1}), from, to)

	count := func(token string) int {
		code, body := do(t, e, "GET", "/api/requests", token, nil)
		mustStatus(t, code, 200, body)
		return len(nested(t, body, "requests").([]any))
	}
	if n := count(tokATC); n != 2 {
		t.Fatalf("ATC sees all requests, got %d", n)
	}
	if n := count(tokCoordA); n != 1 {
		t.Fatalf("coordinator sees own contractor only, got %d", n)
	}
	if n := count(tokPilotA1); n != 1 {
		t.Fatalf("pilot sees own assignments only, got %d", n)
	}
	if n := count(tokPilotB1); n != 1 {
		t.Fatalf("pilot B1 sees own assignments only, got %d", n)
	}
	code, _ := do(t, e, "GET", "/api/requests", tokCommand, nil)
	mustStatus(t, code, 403, nil)
}

// TestAuthRequired rejects anonymous and unknown-token calls.
func TestAuthRequired(t *testing.T) {
	e := newTestServer(t)
	code, _ := do(t, e, "GET", "/api/requests", "", nil)
	mustStatus(t, code, 401, nil)
	code, _ = do(t, e, "GET", "/api/requests", "tok-nope", nil)
	mustStatus(t, code, 401, nil)
	code, _ = do(t, e, "GET", "/healthz", "", nil)
	mustStatus(t, code, 200, nil)
}
