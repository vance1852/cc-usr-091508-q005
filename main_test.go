package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- geometry unit tests ----------

func rectGeom(x0, y0, x1, y1 float64) *Geom {
	g, _ := ParseGeom(mustRect(x0, y0, x1, y1))
	return g
}

func mustRect(x0, y0, x1, y1 float64) json.RawMessage {
	return json.RawMessage(`{"type":"Polygon","coordinates":[[[` +
		ftoa(x0) + `,` + ftoa(y0) + `],[` + ftoa(x1) + `,` + ftoa(y0) + `],[` +
		ftoa(x1) + `,` + ftoa(y1) + `],[` + ftoa(x0) + `,` + ftoa(y1) + `],[` +
		ftoa(x0) + `,` + ftoa(y0) + `]]]}`)
}

func ftoa(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRejectSelfIntersecting(t *testing.T) {
	// Bowtie: (0,0)-(2,2)-(2,0)-(0,2)-(0,0)
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[2,2],[2,0],[0,2],[0,0]]]}`)
	if _, err := ParseGeom(raw); err == nil {
		t.Fatal("expected self-intersection rejection")
	}
}

func TestIntersectionPoints(t *testing.T) {
	a := rectGeom(0, 0, 2, 2)
	b := rectGeom(1, 1, 3, 3)
	pts := IntersectionPoints(a, b)
	if len(pts) == 0 {
		t.Fatal("expected overlap points")
	}
	// The shared corner (1,1) must be present.
	found := false
	for _, p := range pts {
		if p.Lng == 1 && p.Lat == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("corner (1,1) missing from %+v", pts)
	}
}

func TestSuggestSafeCutsCorridor(t *testing.T) {
	// Requested square y[20..25]; horizontal blocker corridor y[21..22].
	req := rectGeom(0, 20, 1, 25)
	blocker := rectGeom(-1, 21, 2, 22)
	safe, err := SuggestSafe(req, []*Geom{blocker})
	if err != nil {
		t.Fatal(err)
	}
	if len(safe.Rings) == 0 {
		t.Fatal("expected non-empty safe area above/below corridor")
	}
	// Result must stay inside requested and clear the blocker.
	for _, r := range safe.Rings {
		if err := ValidateRing(r); err != nil {
			t.Fatalf("safe ring invalid: %v", err)
		}
		for _, p := range r {
			if !GeomContainsPoint(req, p) {
				t.Fatalf("safe point %v leaks outside requested", p)
			}
			if GeomContainsPoint(blocker, p) {
				t.Fatalf("safe point %v still inside blocker", p)
			}
		}
	}
	// A point in the middle of the retained upper band must be covered.
	if !GeomContainsPoint(safe, Point{0.5, 24}) {
		t.Fatal("upper safe band not covered by suggestion")
	}
	if GeomIntersects(safe, blocker) {
		t.Fatal("suggested safe area still intersects blocker")
	}
}

// ---------- end-to-end API scenario ----------

const (
	keyATC     = "atc-key-0001"
	keyRescue  = "rescue-key-0001"
	keyCoordA  = "coord-alpha-0001"
	keyCoordB  = "coord-bravo-0001"
	keyPilotA1 = "pilot-alpha-a1"
	keyPilotA2 = "pilot-alpha-a2"
	keyPilotB1 = "pilot-bravo-b1"
)

type harness struct {
	ts *httptest.Server
	t  *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed(store); err != nil {
		t.Fatal(err)
	}
	return &harness{ts: httptest.NewServer(newServer(store)), t: t}
}

func (h *harness) close() { h.ts.Close() }

func (h *harness) do(method, path, key, idem string, body any) (int, http.Header, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rdr)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func (h *harness) mustJSON(status int, data []byte, v any) {
	h.t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		h.t.Fatalf("status %d: bad json %s: %s", status, string(data), err)
	}
}

type apiPermit struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	AircraftID string `json:"aircraftId"`
}

func window() (string, string) {
	return time.Now().Add(-time.Hour).Format(time.RFC3339),
		time.Now().Add(5 * time.Hour).Format(time.RFC3339)
}

func (h *harness) apply(key, aircraft, mission string, poly json.RawMessage, caps ...string) (int, []byte) {
	from, until := window()
	status, _, data := h.do(http.MethodPost, "/api/permits", key, "idem-"+mission+"-"+aircraft, map[string]any{
		"aircraftId":           aircraft,
		"missionName":          mission,
		"polygon":              json.RawMessage(poly),
		"validFrom":            from,
		"validUntil":           until,
		"altitudeMin":          10,
		"altitudeMax":          90,
		"requiredCapabilities": caps,
	})
	return status, data
}

func (h *harness) decide(permitID, action string, poly json.RawMessage) (int, []byte) {
	body := map[string]any{"action": action, "reason": "测试裁决 " + action}
	if poly != nil {
		body["polygon"] = json.RawMessage(poly)
	}
	status, hdr, data := h.do(http.MethodPost, "/api/permits/"+permitID+"/decisions",
		keyATC, "dec-"+permitID+"-"+action, body)
	if hdr.Get("Idempotent-Replayed") != "false" && status == 200 {
		h.t.Fatalf("decision replay flag wrong: %q", hdr.Get("Idempotent-Replayed"))
	}
	return status, data
}

func credToken(detail map[string]any) string {
	creds := detail["credentials"].([]any)
	if len(creds) == 0 {
		return ""
	}
	last := creds[len(creds)-1].(map[string]any)
	if t, ok := last["token"].(string); ok {
		return t
	}
	return ""
}

func TestFullEmergencyScenario(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	// Geometry: rescue corridor v1 covers x[113.00..113.20] y[23.10..23.16].
	// P1 (uav-a1) works a safe box north-west of it.
	p1Poly := mustRect(113.00, 23.20, 113.05, 23.25)

	// 1. Capability gate: uav-a2 has no lidar.
	st, data := h.apply(keyCoordA, "uav-a2", "堤防激光扫描", p1Poly, "lidar")
	if st != 422 {
		t.Fatalf("want 422 capability, got %d %s", st, data)
	}

	// 2. Bad geometry rejected.
	from, until := window()
	st, _, data = h.do(http.MethodPost, "/api/permits", keyCoordA, "idem-bad", map[string]any{
		"aircraftId": "uav-a1", "missionName": "坏多边形",
		"polygon":   json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[2,2],[2,0],[0,2],[0,0]]]}`),
		"validFrom": from, "validUntil": until,
		"altitudeMin": 10, "altitudeMax": 90, "requiredCapabilities": []string{"optical"},
	})
	if st != 400 {
		t.Fatalf("want 400 geometry, got %d %s", st, data)
	}

	// 3. Cross-contractor aircraft reference refused.
	st, _ = h.apply(keyCoordB, "uav-a1", "越权申请", p1Poly, "optical")
	if st != 403 {
		t.Fatalf("want 403 cross-contractor, got %d", st)
	}

	// 4. File P1 properly.
	st, data = h.apply(keyCoordA, "uav-a1", "一号堤段水情侦察", p1Poly, "optical")
	if st != 201 {
		t.Fatalf("apply P1: %d %s", st, data)
	}
	var p1Detail map[string]any
	h.mustJSON(st, data, &p1Detail)
	p1ID := p1Detail["id"].(string)
	if p1Detail["state"].(string) != "PENDING" {
		t.Fatal("P1 should be PENDING")
	}
	if n := len(p1Detail["versions"].([]any)); n != 1 {
		t.Fatalf("want 1 version, got %d", n)
	}

	// 5. Trying to approve a polygon crossing the rescue corridor is refused
	//    with conflict locations and an advisory safe polygon. (P4 below is
	//    the overlap case; here directly test route block via P1-route overlap:
	//    shrink-style approve is not how approve works, so create P2 crossing.)
	crossing := mustRect(113.02, 23.12, 113.06, 23.22)
	st, data = h.apply(keyCoordA, "uav-a2", "跨航线拍摄", crossing, "optical")
	if st != 201 {
		t.Fatalf("apply crossing: %d %s", st, data)
	}
	var crossPermit apiPermit
	h.mustJSON(st, data, &crossPermit)
	st, data = h.decide(crossPermit.ID, "approve", nil)
	if st != 422 {
		t.Fatalf("want 422 route conflict, got %d %s", st, data)
	}
	var blocked map[string]any
	h.mustJSON(st, data, &blocked)
	if len(blocked["conflicts"].([]any)) == 0 {
		t.Fatal("conflict locations missing")
	}
	if blocked["suggestedSafePolygon"] == nil {
		t.Fatal("advisory safe polygon missing")
	}
	// ATC denies that application.
	st, _ = h.decide(crossPermit.ID, "deny", nil)
	if st != 200 {
		t.Fatalf("deny: %d", st)
	}

	// 6. Approve P1: single-use credential issued.
	st, data = h.decide(p1ID, "approve", nil)
	if st != 200 {
		t.Fatalf("approve P1: %d %s", st, data)
	}
	var dec map[string]any
	h.mustJSON(st, data, &dec)
	token1, _ := dec["credential"].(map[string]any)["token"].(string)
	if token1 == "" {
		t.Fatal("credential token missing from ATC decision")
	}

	// 7. Pilot sees own permit; other contractor / other pilot cannot.
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p1ID, keyPilotB1, "", nil)
	if st != 404 {
		t.Fatalf("bravo pilot must not see alpha permit, got %d", st)
	}
	st, _, _ = h.do(http.MethodGet, "/api/permits/"+p1ID, keyCoordB, "", nil)
	if st != 404 {
		t.Fatalf("bravo coordinator must not see alpha permit, got %d", st)
	}

	// 8. Redeem: single use, idempotent duplicate receipts replay uniquely.
	redeem := func(key, idem string) (int, http.Header, []byte) {
		return h.do(http.MethodPost, "/api/credentials/"+token1+"/redeem", key, idem,
			map[string]string{"note": "起飞前兑换"})
	}
	st, hdr, data := redeem(keyPilotA1, "takeoff-1")
	if st != 200 {
		t.Fatalf("redeem: %d %s", st, data)
	}
	var rr map[string]any
	h.mustJSON(st, data, &rr)
	segmentID := rr["segmentId"].(string)
	if hdr.Get("Idempotent-Replayed") != "false" {
		t.Fatal("first redeem should not be a replay")
	}
	// Delayed/duplicate receipt arrives with the same Idempotency-Key: same
	// unique response, no second redemption.
	st, hdr, data = redeem(keyPilotA1, "takeoff-1")
	if st != 200 || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("duplicate receipt must replay: %d %s", st, data)
	}
	var rr2 map[string]any
	h.mustJSON(st, data, &rr2)
	if rr2["segmentId"] != segmentID {
		t.Fatal("replayed receipt created a different segment")
	}
	// Missing key is rejected; a genuine second attempt is 409 single-use.
	st, _, _ = h.do(http.MethodPost, "/api/credentials/"+token1+"/redeem", keyPilotA1, "", nil)
	if st != 400 {
		t.Fatalf("missing Idempotency-Key want 400, got %d", st)
	}
	st, _, _ = redeem(keyPilotA1, "takeoff-2")
	if st != 409 {
		t.Fatalf("second redemption want 409, got %d", st)
	}
	// Token is bound to its holder.
	st, _, _ = redeem(keyPilotA2, "takeoff-x")
	if st != 403 {
		t.Fatalf("other pilot redeem want 403, got %d", st)
	}

	// 9. In-flight segment: deviation computed server-side against the exact
	//    authorized version; closing again can never rewrite it.
	track := [][]float64{
		{113.01, 23.21}, {113.02, 23.22},
		{113.02, 23.29}, // clearly outside the P1 box (y up to 23.25)
		{113.03, 23.22},
	}
	st, _, data = h.do(http.MethodPost, "/api/segments/"+segmentID, keyPilotA1, "seg-close-1",
		map[string]any{
			"track":       track,
			"deviation":   false, // client claim must be ignored
			"disposition": "发现救援直升机活动，按指令爬高绕飞后归位",
		})
	if st != 200 {
		t.Fatalf("close segment: %d %s", st, data)
	}
	var seg map[string]any
	h.mustJSON(st, data, &seg)
	if seg["deviation"] != true {
		t.Fatalf("server must compute deviation from track, got %v", seg["deviation"])
	}
	if seg["immutable"] != true {
		t.Fatal("closed segment must be marked immutable")
	}
	// Duplicate / corrective receipts cannot rewrite the truth.
	st, _, _ = h.do(http.MethodPost, "/api/segments/"+segmentID, keyPilotA1, "seg-close-2",
		map[string]any{"track": [][]float64{{113.01, 23.21}}, "deviation": false})
	if st != 409 {
		t.Fatalf("rewriting closed segment want 409, got %d", st)
	}

	// 10. P2 for uav-a2 east of P1, clear of v1 corridor; approve it.
	p2Poly := mustRect(113.10, 23.18, 113.15, 23.25)
	st, data = h.apply(keyCoordA, "uav-a2", "二号堤段航拍", p2Poly, "optical")
	if st != 201 {
		t.Fatalf("apply P2: %d %s", st, data)
	}
	var p2 apiPermit
	h.mustJSON(st, data, &p2)
	st, data = h.decide(p2.ID, "approve", nil)
	if st != 200 {
		t.Fatalf("approve P2: %d %s", st, data)
	}
	var d2 map[string]any
	h.mustJSON(st, data, &d2)
	token2 := d2["credential"].(map[string]any)["token"].(string)

	// 11. Rescue route is updated to v2, cutting across P2: the issued
	//     credential freezes immediately and a confirmable notice is left.
	st, _, data = h.do(http.MethodPut, "/api/routes/rt-rescue-main", keyATC, "route-v2", map[string]any{
		"name":     "救援直升机主航线",
		"corridor": json.RawMessage(mustRect(113.10, 23.10, 113.30, 23.22)),
	})
	if st != 200 {
		t.Fatalf("route update: %d %s", st, data)
	}
	var rv map[string]any
	h.mustJSON(st, data, &rv)
	if rv["version"].(float64) != 2 {
		t.Fatalf("route version should be 2, got %v", rv["version"])
	}
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p2.ID, keyPilotA2, "", nil)
	h.mustJSON(st, data, &d2)
	cred := d2["credentials"].([]any)[0].(map[string]any)
	if cred["status"] != "FROZEN" || cred["freezeReason"] != "RESCUE_ROUTE" {
		t.Fatalf("P2 credential should be frozen by route update: %v", cred)
	}
	// Frozen token cannot be redeemed.
	st, _, _ = h.do(http.MethodPost, "/api/credentials/"+token2+"/redeem", keyPilotA2, "redeem-frozen", nil)
	if st != 423 {
		t.Fatalf("frozen redeem want 423, got %d", st)
	}
	// Holder gets the notice and confirms it.
	st, _, data = h.do(http.MethodGet, "/api/notifications", keyPilotA2, "", nil)
	var notifs []map[string]any
	h.mustJSON(st, data, &notifs)
	var freezeNotifID string
	for _, n := range notifs {
		if n["type"] == "RESCUE_ROUTE_UPDATE" {
			freezeNotifID = n["id"].(string)
		}
	}
	if freezeNotifID == "" {
		t.Fatal("freeze notice missing")
	}
	st, _, _ = h.do(http.MethodPost, "/api/notifications/"+freezeNotifID+"/ack", keyPilotA2, "", nil)
	if st != 200 {
		t.Fatalf("ack: %d", st)
	}
	// Another pilot cannot acknowledge someone else's notice.
	st, _, _ = h.do(http.MethodPost, "/api/notifications/"+freezeNotifID+"/ack", keyPilotA1, "", nil)
	if st != 403 {
		t.Fatalf("cross-pilot ack want 403, got %d", st)
	}

	// 12. ATC shrinks P2 to the clear strip (with a gap from the corridor):
	//     old credential stays frozen, a brand-new single-use credential is
	//     issued against v3, and the unique active version switches.
	shrunk := mustRect(113.10, 23.225, 113.15, 23.25)
	st, data = h.decide(p2.ID, "shrink", shrunk)
	if st != 200 {
		t.Fatalf("shrink P2: %d %s", st, data)
	}
	h.mustJSON(st, data, &d2)
	token3 := d2["credential"].(map[string]any)["token"].(string)
	if token3 == "" || token3 == token2 {
		t.Fatal("shrink must issue a fresh credential")
	}
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p2.ID, keyATC, "", nil)
	h.mustJSON(st, data, &d2)
	vers := d2["versions"].([]any)
	activeCount := 0
	for _, v := range vers {
		if v.(map[string]any)["active"] == true {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("exactly one active version required, got %d", activeCount)
	}
	if len(vers) != 3 {
		t.Fatalf("want full version history (draft/v2/v3), got %d", len(vers))
	}

	// 13. Aircraft link loss freezes usable credentials immediately.
	st, _, _ = h.do(http.MethodPost, "/api/aircraft/uav-a2/link", keyCoordA, "link-lost-a2",
		map[string]string{"status": "lost"})
	if st != 200 {
		t.Fatalf("link report: %d", st)
	}
	st, _, _ = h.do(http.MethodPost, "/api/credentials/"+token3+"/redeem", keyPilotA2, "redeem-lost", nil)
	if st != 403 && st != 423 {
		t.Fatalf("lost-link redeem must be refused, got %d", st)
	}

	// 14. Pending permit vs approved permit: the application is visible as a
	//     conflict but must not freeze the approved neighbor's credential.
	//     P3 sits clear east of the v2 corridor (which ends at x=113.30).
	p3Poly := mustRect(113.31, 23.18, 113.36, 23.22)
	st, data = h.apply(keyCoordB, "uav-b1", "三号堤段热成像", p3Poly, "thermal")
	if st != 201 {
		t.Fatalf("apply P3: %d %s", st, data)
	}
	var p3 apiPermit
	h.mustJSON(st, data, &p3)
	if st, data = h.decide(p3.ID, "approve", nil); st != 200 {
		t.Fatalf("approve P3: %d %s", st, data)
	}
	overlap := mustRect(113.31, 23.20, 113.34, 23.25)
	st, data = h.apply(keyCoordB, "uav-b1", "与三号重叠的任务", overlap, "optical")
	if st != 201 {
		t.Fatalf("apply P4: %d %s", st, data)
	}
	var p4 apiPermit
	h.mustJSON(st, data, &p4)
	// Approving P4 is blocked; response carries exact conflict points and the
	// safe remainder (strip above P3's ceiling at 23.22).
	st, data = h.decide(p4.ID, "approve", nil)
	if st != 422 {
		t.Fatalf("overlap approve want 422, got %d %s", st, data)
	}
	var ov map[string]any
	h.mustJSON(st, data, &ov)
	if cf := ov["conflicts"].([]any); len(cf) != 1 {
		t.Fatalf("want exactly one conflict, got %d", len(cf))
	}
	safeRaw, _ := json.Marshal(ov["suggestedSafePolygon"])
	safeG, err := ParseGeom(safeRaw)
	if err != nil {
		t.Fatalf("safe polygon invalid: %v", err)
	}
	if !GeomContainsPoint(safeG, Point{113.315, 23.24}) {
		t.Fatal("safe strip above neighbor should be suggested")
	}
	// P3 credential is untouched while P4 is merely pending.
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p3.ID, keyATC, "", nil)
	var p3d map[string]any
	h.mustJSON(st, data, &p3d)
	for _, c := range p3d["credentials"].([]any) {
		if c.(map[string]any)["status"] != "ISSUED" {
			t.Fatalf("pending application must not freeze approved neighbor: %v", c)
		}
	}
	// Deny P4: conflict clears.
	st, _ = h.decide(p4.ID, "deny", nil)
	if st != 200 {
		t.Fatalf("deny P4: %d", st)
	}

	// 15. Revocation freezes the credential and notifies the holder.
	st, _ = h.decide(p3.ID, "revoke", nil)
	if st != 200 {
		t.Fatalf("revoke P3: %d", st)
	}
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p3.ID, keyPilotB1, "", nil)
	h.mustJSON(st, data, &p3d)
	if p3d["state"] != "REVOKED" {
		t.Fatal("P3 should be REVOKED")
	}
	for _, c := range p3d["credentials"].([]any) {
		if c.(map[string]any)["status"] != "FROZEN" ||
			c.(map[string]any)["freezeReason"] != "REVOKED" {
			t.Fatalf("revoked permit credential must be frozen REVOKED: %v", c)
		}
	}

	// 16. Rescue command sees the conflict overview; pilots do not.
	st, _, data = h.do(http.MethodGet, "/api/dashboard/rescue", keyRescue, "", nil)
	if st != 200 {
		t.Fatalf("dashboard: %d", st)
	}
	var dash map[string]any
	h.mustJSON(st, data, &dash)
	if _, ok := dash["openConflicts"]; !ok {
		t.Fatal("dashboard missing openConflicts")
	}
	if _, ok := dash["pendingAcknowledgements"]; !ok {
		t.Fatal("dashboard must list who still has to respond")
	}
	st, _, _ = h.do(http.MethodGet, "/api/dashboard/rescue", keyPilotA1, "", nil)
	if st != 403 {
		t.Fatalf("pilot dashboard want 403, got %d", st)
	}

	// 17. Idempotency key reused with a different body is a 409, never retried.
	st, _, _ = h.do(http.MethodPost, "/api/permits", keyCoordA, "idem-reuse", map[string]any{
		"aircraftId": "uav-a1", "missionName": "第一次",
		"polygon":   p1Poly,
		"validFrom": from, "validUntil": until,
		"altitudeMin": 10, "altitudeMax": 90,
	})
	if st != 201 {
		t.Fatalf("first create: %d", st)
	}
	st, _, _ = h.do(http.MethodPost, "/api/permits", keyCoordA, "idem-reuse", map[string]any{
		"aircraftId": "uav-a1", "missionName": "第二次-不同载荷",
		"polygon":   p1Poly,
		"validFrom": from, "validUntil": until,
		"altitudeMin": 10, "altitudeMax": 90,
	})
	if st != 409 {
		t.Fatalf("reused idempotency key want 409, got %d", st)
	}

	// 18. Audit trail records every version and decision source.
	st, _, data = h.do(http.MethodGet, "/api/events", keyATC, "", nil)
	if st != 200 {
		t.Fatalf("events: %d", st)
	}
	if !strings.Contains(string(data), "FREEZE_ENGINE") ||
		!strings.Contains(string(data), "ATC_DECISION") ||
		!strings.Contains(string(data), "ROUTE_VERSION") {
		t.Fatal("audit trail missing decision sources")
	}

	// 19. Unauthenticated requests are rejected.
	st, _, _ = h.do(http.MethodGet, "/api/permits", "", "", nil)
	if st != 401 {
		t.Fatalf("want 401, got %d", st)
	}
}

func TestExpiredWindowRejectsRedeem(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	poly := mustRect(113.00, 23.20, 113.05, 23.25)
	from := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	until := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	st, _, data := h.do(http.MethodPost, "/api/permits", keyCoordA, "expired-apply", map[string]any{
		"aircraftId":  "uav-a1",
		"missionName": "窗口已过的任务",
		"polygon":     json.RawMessage(poly),
		"validFrom":   from, "validUntil": until,
		"altitudeMin": 10, "altitudeMax": 90,
		"requiredCapabilities": []string{"optical"},
	})
	if st != 201 {
		t.Fatalf("apply: %d %s", st, data)
	}
	var p map[string]any
	h.mustJSON(st, data, &p)
	st, data = h.decide(p["id"].(string), "approve", nil)
	if st != 200 {
		t.Fatalf("approve: %d %s", st, data)
	}
	var d map[string]any
	h.mustJSON(st, data, &d)
	tok := d["credential"].(map[string]any)["token"].(string)

	st, _, data = h.do(http.MethodPost, "/api/credentials/"+tok+"/redeem", keyPilotA1, "expired-redeem", nil)
	if st != 410 {
		t.Fatalf("redeem outside window want 410, got %d %s", st, data)
	}
}

func TestInflightRouteUpdateWarnsButKeepsSegment(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	// P1 box clear of v1 corridor; approve and redeem — aircraft is airborne.
	st, data := h.apply(keyCoordA, "uav-a1", "在飞任务",
		mustRect(113.00, 23.20, 113.05, 23.25), "optical")
	if st != 201 {
		t.Fatalf("apply: %d %s", st, data)
	}
	var p map[string]any
	h.mustJSON(st, data, &p)
	st, data = h.decide(p["id"].(string), "approve", nil)
	if st != 200 {
		t.Fatalf("approve: %d %s", st, data)
	}
	var d map[string]any
	h.mustJSON(st, data, &d)
	tok := d["credential"].(map[string]any)["token"].(string)
	st, _, data = h.do(http.MethodPost, "/api/credentials/"+tok+"/redeem", keyPilotA1, "if-1", nil)
	if st != 200 {
		t.Fatalf("redeem: %d %s", st, data)
	}
	var rr map[string]any
	h.mustJSON(st, data, &rr)
	segID := rr["segmentId"].(string)

	// Route v2 now cuts across the airborne mission box.
	st, _, data = h.do(http.MethodPut, "/api/routes/rt-rescue-main", keyATC, "if-route", map[string]any{
		"name":     "救援直升机主航线",
		"corridor": json.RawMessage(mustRect(113.00, 23.10, 113.10, 23.23)),
	})
	if st != 200 {
		t.Fatalf("route update: %d %s", st, data)
	}

	// Credential is NOT retroactively frozen; it stays REDEEMED...
	st, _, data = h.do(http.MethodGet, "/api/permits/"+p["id"].(string), keyPilotA1, "", nil)
	if st != 200 {
		t.Fatalf("permit: %d", st)
	}
	h.mustJSON(st, data, &p)
	cred := p["credentials"].([]any)[0].(map[string]any)
	if cred["status"] != "REDEEMED" {
		t.Fatalf("in-flight credential must stay REDEEMED, got %v", cred["status"])
	}
	// ...but the holder has exactly one confirmable avoidance warning.
	st, _, data = h.do(http.MethodGet, "/api/notifications", keyPilotA1, "", nil)
	var notifs []map[string]any
	h.mustJSON(st, data, &notifs)
	warns := 0
	var warnID string
	for _, n := range notifs {
		if n["type"] == "INFLIGHT_AVOIDANCE" {
			warns++
			warnID = n["id"].(string)
		}
	}
	if warns != 1 {
		t.Fatalf("want exactly one in-flight avoidance notice, got %d", warns)
	}
	st, _, _ = h.do(http.MethodPost, "/api/notifications/"+warnID+"/ack", keyPilotA1, "", nil)
	if st != 200 {
		t.Fatalf("ack: %d", st)
	}

	// The flight can still truthfully close its segment; deviation is computed
	// against its bound version and is immutable afterwards.
	st, _, data = h.do(http.MethodPost, "/api/segments/"+segID, keyPilotA1, "if-seg", map[string]any{
		"track":       [][]float64{{113.01, 23.21}, {113.02, 23.22}},
		"disposition": "按新航线指令保持在走廊以北，正常归位",
	})
	if st != 200 {
		t.Fatalf("close segment: %d %s", st, data)
	}
	var seg map[string]any
	h.mustJSON(st, data, &seg)
	if seg["deviation"] != false || seg["immutable"] != true {
		t.Fatalf("compliant in-flight track: %v", seg)
	}
}
