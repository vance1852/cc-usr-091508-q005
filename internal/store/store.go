// Package store owns the SQLite schema and the row models. Every
// authorization decision is kept as an immutable version row so the full
// history of polygon boundaries and decision origins survives.
package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"airspace/internal/geo"

	_ "github.com/mattn/go-sqlite3"
)

// TimeFormat is how timestamps are persisted as TEXT.
const TimeFormat = time.RFC3339Nano

// Roles.
const (
	RoleCoordinator = "coordinator" // 应急测绘协调员
	RolePilot       = "pilot"       // 飞手
	RoleATC         = "atc"         // 空管员
	RoleCommand     = "command"     // 救援指挥
)

// Version decisions.
const (
	DecisionApproved = "approved"
	DecisionShrunk   = "shrunk"
	DecisionRevoked  = "revoked"
)

// Credential states.
const (
	CredActive   = "active"
	CredFrozen   = "frozen"
	CredConsumed = "consumed"
	CredRevoked  = "revoked"
)

// Freeze reasons (also recorded on notifications).
const (
	FreezeRouteConflict = "route_conflict"      // 与救援航线走廊相交
	FreezeLinkLost      = "link_lost"           // 飞行器失联
	FreezeMutual        = "mutual_intersection" // 与其他在飞授权范围相交
)

// Notification kinds.
const (
	NotifDecision = "decision"            // 新许可版本已签发
	NotifFrozen   = "credential_frozen"   // 凭证被冻结
	NotifRevoked  = "credential_revoked"  // 凭证被撤销
	NotifUnfrozen = "credential_unfrozen" // 凭证被恢复
)

type User struct {
	ID           int64
	Name         string
	Role         string
	ContractorID *int64
	Token        string
}

type Aircraft struct {
	ID           int64
	Callsign     string
	ContractorID int64
	Capabilities json.RawMessage
	LinkStatus   string
	UpdatedAt    time.Time
}

type RescueRoute struct {
	ID        int64
	Name      string
	Corridor  geo.Polygon
	Active    bool
	Version   int
	UpdatedBy *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Request struct {
	ID           int64
	RequesterID  int64
	ContractorID int64
	Area         geo.Polygon
	ValidFrom    time.Time
	ValidTo      time.Time
	Capabilities json.RawMessage
	Purpose      string
	CreatedAt    time.Time
}

type RequestAircraft struct {
	RequestID  int64
	AircraftID int64
	PilotID    int64
}

// Version is one immutable decision on a request: the polygon boundary
// as of that version plus who/what decided it.
type Version struct {
	ID             int64
	RequestID      int64
	Version        int
	Polygon        geo.Polygon
	ValidFrom      time.Time
	ValidTo        time.Time
	Decision       string
	DecidedBy      int64
	DecisionSource string // manual | rule:avoidance
	Reason         string
	CreatedAt      time.Time
}

type Credential struct {
	ID           int64
	VersionID    int64
	RequestID    int64
	AircraftID   int64
	PilotID      int64
	Token        string
	Status       string
	IssuedAt     time.Time
	UsedAt       *time.Time
	FrozenAt     *time.Time
	FrozenReason string
}

type Notification struct {
	ID           int64
	UserID       int64
	RequestID    *int64
	CredentialID *int64
	Kind         string
	Message      string
	CreatedAt    time.Time
	AckedAt      *time.Time
	AckReceipt   *string
}

// Segment is a flown leg reported after takeoff. Rows are insert-only:
// a flown segment can never be rewritten as unexecuted.
type Segment struct {
	ID              int64
	CredentialID    int64
	RequestID       int64
	AircraftID      int64
	PilotID         int64
	StartedAt       time.Time
	EndedAt         *time.Time
	Deviated        bool
	DeviationReason string
	DeviationAction string
	ReportedAt      time.Time
}

// Open opens (and creates if needed) the SQLite database. A single
// connection is used: SQLite serializes writers anyway, and one
// connection keeps in-memory databases and transactions unambiguous.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS contractors(
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS users(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,
  role          TEXT NOT NULL CHECK(role IN ('coordinator','pilot','atc','command')),
  contractor_id INTEGER REFERENCES contractors(id),
  token         TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS aircraft(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  callsign      TEXT NOT NULL UNIQUE,
  contractor_id INTEGER NOT NULL REFERENCES contractors(id),
  capabilities  TEXT NOT NULL DEFAULT '{}',
  link_status   TEXT NOT NULL DEFAULT 'ok' CHECK(link_status IN ('ok','lost')),
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rescue_routes(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  corridor   TEXT NOT NULL,           -- polygon JSON
  active     INTEGER NOT NULL DEFAULT 1,
  version    INTEGER NOT NULL DEFAULT 1,
  updated_by INTEGER REFERENCES users(id),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS requests(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  requester_id  INTEGER NOT NULL REFERENCES users(id),
  contractor_id INTEGER NOT NULL REFERENCES contractors(id),
  area          TEXT NOT NULL,        -- polygon JSON (requested)
  valid_from    TEXT NOT NULL,
  valid_to      TEXT NOT NULL,
  capabilities  TEXT NOT NULL DEFAULT '{}',
  purpose       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS request_aircraft(
  request_id  INTEGER NOT NULL REFERENCES requests(id),
  aircraft_id INTEGER NOT NULL REFERENCES aircraft(id),
  pilot_id    INTEGER NOT NULL REFERENCES users(id),
  PRIMARY KEY(request_id, aircraft_id)
);

CREATE TABLE IF NOT EXISTS auth_versions(
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id      INTEGER NOT NULL REFERENCES requests(id),
  version         INTEGER NOT NULL,
  polygon         TEXT NOT NULL,      -- polygon JSON as decided
  valid_from      TEXT NOT NULL,
  valid_to        TEXT NOT NULL,
  decision        TEXT NOT NULL CHECK(decision IN ('approved','shrunk','revoked')),
  decided_by      INTEGER NOT NULL REFERENCES users(id),
  decision_source TEXT NOT NULL,      -- manual | rule:avoidance
  reason          TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  UNIQUE(request_id, version)
);

CREATE TABLE IF NOT EXISTS credentials(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  version_id    INTEGER NOT NULL REFERENCES auth_versions(id),
  request_id    INTEGER NOT NULL REFERENCES requests(id),
  aircraft_id   INTEGER NOT NULL REFERENCES aircraft(id),
  pilot_id      INTEGER NOT NULL REFERENCES users(id),
  token         TEXT NOT NULL UNIQUE,
  status        TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','frozen','consumed','revoked')),
  issued_at     TEXT NOT NULL,
  used_at       TEXT,
  frozen_at     TEXT,
  frozen_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cred_request ON credentials(request_id);
CREATE INDEX IF NOT EXISTS idx_cred_pilot   ON credentials(pilot_id);

CREATE TABLE IF NOT EXISTS notifications(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  request_id    INTEGER REFERENCES requests(id),
  credential_id INTEGER REFERENCES credentials(id),
  kind          TEXT NOT NULL,
  message       TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  acked_at      TEXT,
  ack_receipt   TEXT
);
-- A receipt id may confirm exactly one notification; duplicate deliveries
-- of the same receipt are answered from the stored row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notif_receipt
  ON notifications(ack_receipt) WHERE ack_receipt IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notif_user ON notifications(user_id, acked_at);

CREATE TABLE IF NOT EXISTS segments(
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  credential_id    INTEGER NOT NULL REFERENCES credentials(id),
  request_id       INTEGER NOT NULL REFERENCES requests(id),
  aircraft_id      INTEGER NOT NULL REFERENCES aircraft(id),
  pilot_id         INTEGER NOT NULL REFERENCES users(id),
  started_at       TEXT NOT NULL,
  ended_at         TEXT,
  deviated         INTEGER NOT NULL DEFAULT 0,
  deviation_reason TEXT NOT NULL DEFAULT '',
  deviation_action TEXT NOT NULL DEFAULT '',
  reported_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_seg_request ON segments(request_id);
`

// Migrate creates the schema if it does not exist yet.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// --- small shared helpers ---

// FmtTime renders t for storage.
func FmtTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a stored timestamp.
func ParseTime(s string) time.Time {
	t, _ := time.Parse(TimeFormat, s)
	return t
}

// MarshalPolygon encodes a polygon for the TEXT columns.
func MarshalPolygon(p geo.Polygon) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// UnmarshalPolygon decodes a stored polygon.
func UnmarshalPolygon(s string) geo.Polygon {
	var p geo.Polygon
	_ = json.Unmarshal([]byte(s), &p)
	return p
}
