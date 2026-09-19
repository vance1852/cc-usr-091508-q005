package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db  *sql.DB
	mu  sync.Mutex // serialize writers; SQLite file + WAL
	now func() time.Time
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func newToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Writers are serialized by withTx's mutex; in WAL mode concurrent read
	// connections are safe and also necessary — a read handler that opens a
	// second query while scanning rows must not deadlock against itself.
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
  api_key       TEXT PRIMARY KEY,
  role          TEXT NOT NULL CHECK(role IN ('coordinator','pilot','rescue','atc')),
  contractor_id TEXT,
  aircraft_id   TEXT,
  display_name  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS aircraft (
  id            TEXT PRIMARY KEY,
  contractor_id TEXT NOT NULL,
  callsign      TEXT NOT NULL,
  capabilities  TEXT NOT NULL DEFAULT '[]',
  link_status   TEXT NOT NULL DEFAULT 'ok' CHECK(link_status IN ('ok','lost')),
  last_seen_at  TEXT
);
CREATE TABLE IF NOT EXISTS permits (
  id               TEXT PRIMARY KEY,
  contractor_id    TEXT NOT NULL,
  aircraft_id      TEXT NOT NULL,
  mission_name     TEXT NOT NULL,
  created_by       TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  state            TEXT NOT NULL CHECK(state IN ('PENDING','APPROVED','DENIED','REVOKED')),
  current_version_id TEXT,
  valid_from       TEXT,
  valid_until      TEXT,
  alt_min          REAL NOT NULL DEFAULT 0,
  alt_max          REAL NOT NULL DEFAULT 120,
  required_capabilities TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS permit_versions (
  id           TEXT PRIMARY KEY,
  permit_id    TEXT NOT NULL REFERENCES permits(id),
  seq          INTEGER NOT NULL,
  kind         TEXT NOT NULL CHECK(kind IN ('DRAFT','APPROVED','SHRUNK','DENIED','REVOKED')),
  source       TEXT NOT NULL,
  polygon      TEXT,
  alt_min      REAL,
  alt_max      REAL,
  valid_from   TEXT,
  valid_until  TEXT,
  required_capabilities TEXT,
  decided_by   TEXT,
  reason       TEXT,
  created_at   TEXT NOT NULL,
  active       INTEGER NOT NULL DEFAULT 0,
  UNIQUE(permit_id, seq)
);
CREATE TABLE IF NOT EXISTS rescue_routes (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  corridor   TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  active     INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS credentials (
  id                   TEXT PRIMARY KEY,
  permit_id            TEXT NOT NULL REFERENCES permits(id),
  version_id           TEXT NOT NULL REFERENCES permit_versions(id),
  aircraft_id          TEXT NOT NULL,
  contractor_id        TEXT NOT NULL,
  token                TEXT NOT NULL UNIQUE,
  status               TEXT NOT NULL CHECK(status IN ('ISSUED','FROZEN','REDEEMED','EXPIRED')),
  freeze_reason        TEXT,
  issued_at            TEXT NOT NULL,
  frozen_at            TEXT,
  redeemed_at          TEXT,
  redeemed_segment_id  TEXT
);
CREATE TABLE IF NOT EXISTS segments (
  id               TEXT PRIMARY KEY,
  credential_id    TEXT NOT NULL REFERENCES credentials(id),
  aircraft_id      TEXT NOT NULL,
  permit_id        TEXT NOT NULL,
  started_at       TEXT NOT NULL,
  ended_at         TEXT,
  track            TEXT,
  deviation        INTEGER NOT NULL DEFAULT 0,
  deviation_detail TEXT,
  disposition      TEXT,
  recorded_at      TEXT NOT NULL,
  recorded_by      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS conflicts (
  id            TEXT PRIMARY KEY,
  kind          TEXT NOT NULL CHECK(kind IN ('PERMIT_PERMIT','PERMIT_ROUTE')),
  permit_a      TEXT NOT NULL REFERENCES permits(id),
  permit_b      TEXT REFERENCES permits(id),
  route_id      TEXT REFERENCES rescue_routes(id),
  route_version INTEGER,
  description   TEXT NOT NULL,
  points        TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  resolved_at   TEXT
);
CREATE TABLE IF NOT EXISTS notifications (
  id            TEXT PRIMARY KEY,
  aircraft_id   TEXT NOT NULL,
  contractor_id TEXT NOT NULL,
  credential_id TEXT REFERENCES credentials(id),
  permit_id     TEXT NOT NULL REFERENCES permits(id),
  type          TEXT NOT NULL,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  acknowledged_at TEXT
);
CREATE TABLE IF NOT EXISTS acknowledgements (
  notification_id TEXT NOT NULL REFERENCES notifications(id),
  api_key         TEXT NOT NULL,
  acked_at        TEXT NOT NULL,
  PRIMARY KEY(notification_id, api_key)
);
CREATE TABLE IF NOT EXISTS events (
  id         TEXT PRIMARY KEY,
  permit_id  TEXT,
  type       TEXT NOT NULL,
  source     TEXT NOT NULL,
  actor      TEXT,
  detail     TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency (
  idem_key    TEXT NOT NULL,
  api_key     TEXT NOT NULL,
  scope       TEXT NOT NULL,
  req_hash    TEXT NOT NULL,
  status_code INTEGER NOT NULL,
  response    TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY(idem_key, api_key, scope)
);
CREATE INDEX IF NOT EXISTS idx_versions_permit ON permit_versions(permit_id);
CREATE INDEX IF NOT EXISTS idx_cred_permit ON credentials(permit_id);
CREATE INDEX IF NOT EXISTS idx_cred_aircraft ON credentials(aircraft_id);
CREATE INDEX IF NOT EXISTS idx_seg_cred ON segments(credential_id);
CREATE INDEX IF NOT EXISTS idx_conf_open ON conflicts(permit_a, resolved_at);
CREATE INDEX IF NOT EXISTS idx_notif_aircraft ON notifications(aircraft_id);
CREATE INDEX IF NOT EXISTS idx_events_permit ON events(permit_id);
`

// withTx serializes all writes behind one mutex so freeze cascades are atomic.
func (s *Store) withTx(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) event(tx *sql.Tx, permitID, typ, source, actor, detail string) {
	_, _ = tx.Exec(
		`INSERT INTO events(id, permit_id, type, source, actor, detail, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		newID("evt"), permitID, typ, source, actor, detail, s.now().Format(time.RFC3339))
}

// ---- user / aircraft lookups ----

func (s *Store) userByKey(key string) (*userRow, error) {
	var u userRow
	err := s.db.QueryRow(
		`SELECT api_key, role, COALESCE(contractor_id,''), COALESCE(aircraft_id,''), display_name
		 FROM users WHERE api_key=?`, key).
		Scan(&u.APIKey, &u.Role, &u.ContractorID, &u.AircraftID, &u.DisplayName)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) aircraft(id string) (*aircraftRow, error) {
	var a aircraftRow
	var caps string
	err := s.db.QueryRow(
		`SELECT id, contractor_id, callsign, capabilities, link_status, COALESCE(last_seen_at,'')
		 FROM aircraft WHERE id=?`, id).
		Scan(&a.ID, &a.ContractorID, &a.Callsign, &caps, &a.LinkStatus, &a.LastSeenAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(caps), &a.Capabilities)
	return &a, nil
}

// ---- idempotency (delayed or duplicate field receipts) ----

type idemReplay struct {
	StatusCode int
	Body       []byte
}

// idemLookup returns a stored response for a repeated receipt. A mismatched
// request body reusing the same key is a client error, never silently retried.
func (s *Store) idemLookup(key, apiKey, scope, reqHash string) (*idemReplay, error) {
	var status int
	var body, hash string
	err := s.db.QueryRow(
		`SELECT status_code, response, req_hash FROM idempotency
		 WHERE idem_key=? AND api_key=? AND scope=?`, key, apiKey, scope).
		Scan(&status, &body, &hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hash != reqHash {
		return &idemReplay{StatusCode: 409, Body: []byte(`{"error":"idempotency key reused with a different request payload"}`)}, nil
	}
	return &idemReplay{StatusCode: status, Body: []byte(body)}, nil
}

func (s *Store) idemStore(key, apiKey, scope, reqHash string, status int, body []byte) {
	_, _ = s.db.Exec(
		`INSERT OR IGNORE INTO idempotency(idem_key, api_key, scope, req_hash, status_code, response, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		key, apiKey, scope, reqHash, status, string(body), s.now().Format(time.RFC3339))
}
