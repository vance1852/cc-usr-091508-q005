package app

import (
	"database/sql"
	"time"

	"airspace/internal/geo"
	"airspace/internal/store"
)

// SeedUser describes one seeded account; tokens are deterministic so the
// demo scenario is reproducible. Change them before any real deployment.
type SeedUser struct {
	Name, Role, Token string
	Contractor        string // empty for ATC / command
}

// SeedIfEmpty populates an empty database with the demo cast: two
// surveying contractors, their coordinators/pilots/aircraft, one ATC
// seat, one rescue-command seat, and one helicopter corridor.
func SeedIfEmpty(db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}

	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	contractors := []string{"曙光测绘队", "堤防快测队"}
	cid := map[string]int64{}
	for _, name := range contractors {
		res, err := tx.Exec(`INSERT INTO contractors(name) VALUES (?)`, name)
		if err != nil {
			return false, err
		}
		id, _ := res.LastInsertId()
		cid[name] = id
	}

	users := []SeedUser{
		{"空管席·林塔", store.RoleATC, "tok-atc", ""},
		{"指挥所·陈指挥", store.RoleCommand, "tok-command", ""},
		{"协调员·周航", store.RoleCoordinator, "tok-coord-alpha", "曙光测绘队"},
		{"协调员·吴堤", store.RoleCoordinator, "tok-coord-beta", "堤防快测队"},
		{"飞手·赵一", store.RolePilot, "tok-pilot-a1", "曙光测绘队"},
		{"飞手·钱二", store.RolePilot, "tok-pilot-a2", "曙光测绘队"},
		{"飞手·孙三", store.RolePilot, "tok-pilot-b1", "堤防快测队"},
	}
	for _, u := range users {
		var cptr *int64
		if u.Contractor != "" {
			id := cid[u.Contractor]
			cptr = &id
		}
		if _, err := tx.Exec(`INSERT INTO users(name, role, contractor_id, token) VALUES (?,?,?,?)`,
			u.Name, u.Role, nullInt64(cptr), u.Token); err != nil {
			return false, err
		}
	}

	aircraft := []struct {
		callsign, contractor, caps string
	}{
		{"UAV-A1", "曙光测绘队", `{"max_altitude_m":120,"endurance_min":45,"sensors":["rgb","thermal"]}`},
		{"UAV-A2", "曙光测绘队", `{"max_altitude_m":100,"endurance_min":30,"sensors":["rgb"]}`},
		{"UAV-B1", "堤防快测队", `{"max_altitude_m":150,"endurance_min":60,"sensors":["rgb","lidar"]}`},
	}
	for _, a := range aircraft {
		if _, err := tx.Exec(`INSERT INTO aircraft(callsign, contractor_id, capabilities, link_status, updated_at)
			VALUES (?,?,?,'ok',?)`, a.callsign, cid[a.contractor], a.caps, store.FmtTime(now)); err != nil {
			return false, err
		}
	}

	// 救援直升机航线「生命线一号」：一条东西向窄走廊。
	corridor := geo.Polygon{Vertices: []geo.Point{
		{Lat: 31.0000, Lng: 120.9800},
		{Lat: 31.0000, Lng: 121.0600},
		{Lat: 31.0040, Lng: 121.0600},
		{Lat: 31.0040, Lng: 120.9800},
	}}
	if _, err := tx.Exec(`INSERT INTO rescue_routes(name, corridor, active, version, updated_by, created_at, updated_at)
		VALUES ('生命线一号', ?, 1, 1, 1, ?, ?)`, store.MarshalPolygon(corridor), store.FmtTime(now), store.FmtTime(now)); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
