package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	path := os.Getenv("FLOODAIR_DB")
	if path == "" {
		path = "floodair.db"
	}
	store, err := OpenStore(path)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if err := seed(store); err != nil {
		log.Fatalf("seed: %v", err)
	}
	e := newServer(store)
	addr := os.Getenv("FLOODAIR_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("floodair temporary airspace authorization service listening on %s (db=%s)", addr, path)
	if err := e.StartServer(&http.Server{Addr: addr}); err != nil {
		log.Fatal(err)
	}
}

// seed provisions users, aircraft and one active rescue helicopter corridor.
// It is idempotent: safe to run against an existing database.
func seed(s *Store) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		users := []struct {
			key, role, contractor, aircraft, name string
		}{
			{"atc-key-0001", "atc", "", "", "空管值班 李管制"},
			{"rescue-key-0001", "rescue", "", "", "救援指挥 周指挥"},
			{"coord-alpha-0001", "coordinator", "ALPHA", "", "甲承包 应急测绘协调员"},
			{"coord-bravo-0001", "coordinator", "BRAVO", "", "乙承包 应急测绘协调员"},
			{"pilot-alpha-a1", "pilot", "ALPHA", "uav-a1", "甲承包飞手 A1"},
			{"pilot-alpha-a2", "pilot", "ALPHA", "uav-a2", "甲承包飞手 A2"},
			{"pilot-bravo-b1", "pilot", "BRAVO", "uav-b1", "乙承包飞手 B1"},
		}
		for _, u := range users {
			if _, err := tx.Exec(
				`INSERT INTO users(api_key, role, contractor_id, aircraft_id, display_name)
				 VALUES(?,?,?,?,?)`, u.key, u.role, nstr(u.contractor), nstr(u.aircraft), u.name); err != nil {
				return err
			}
		}
		mkCaps := func(cs ...string) string { b, _ := json.Marshal(cs); return string(b) }
		aircraft := []struct {
			id, contractor, callsign, caps string
		}{
			{"uav-a1", "ALPHA", "ALPHA-A1", mkCaps("optical", "thermal", "night")},
			{"uav-a2", "ALPHA", "ALPHA-A2", mkCaps("optical")},
			{"uav-b1", "BRAVO", "BRAVO-B1", mkCaps("optical", "thermal", "lidar")},
		}
		for _, a := range aircraft {
			if _, err := tx.Exec(
				`INSERT INTO aircraft(id, contractor_id, callsign, capabilities) VALUES(?,?,?,?)`,
				a.id, a.contractor, a.callsign, a.caps); err != nil {
				return err
			}
		}
		// Initial rescue helicopter corridor — a wide diagonal corridor along
		// the river embankment. Permit applications crossing it must避让.
		corridor := `{"type":"Polygon","coordinates":[[[113.000,23.100],[113.200,23.100],[113.200,23.160],[113.000,23.160],[113.000,23.100]]]}`
		if _, err := tx.Exec(
			`INSERT INTO rescue_routes(id, name, version, corridor, created_by, created_at, active)
			 VALUES('rt-rescue-main','救援直升机主航线',1,?, 'atc-key-0001',
			        strftime('%Y-%m-%dT%H:%M:%SZ','now'),1)`, corridor); err != nil {
			return err
		}
		return nil
	})
}

func nstr(s string) interface{} {
	if s == "" {
		return sql.NullString{}
	}
	return s
}
