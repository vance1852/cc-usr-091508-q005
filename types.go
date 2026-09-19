package main

import "encoding/json"

// All wire timestamps are RFC3339 UTC strings; SQLite stores them as TEXT.

type userRow struct {
	APIKey       string
	Role         string
	ContractorID string `db:"contractor_id"`
	AircraftID   string `db:"aircraft_id"`
	DisplayName  string `db:"display_name"`
}

type aircraftRow struct {
	ID           string   `json:"id"`
	ContractorID string   `json:"contractorId"`
	Callsign     string   `json:"callsign"`
	Capabilities []string `json:"capabilities"`
	LinkStatus   string   `json:"linkStatus"`
	LastSeenAt   string   `json:"lastSeenAt"`
}

type versionRow struct {
	ID                   string
	PermitID             string
	Seq                  int
	Kind                 string // DRAFT | APPROVED | SHRUNK | REVOKED
	Source               string
	Polygon              string // GeoJSON Polygon
	AltMin               float64
	AltMax               float64
	ValidFrom            string
	ValidUntil           string
	RequiredCapabilities string // JSON array
	DecidedBy            string
	CreatedAt            string
	Active               bool
}

type credentialRow struct {
	ID                string
	PermitID          string
	VersionID         string
	AircraftID        string
	ContractorID      string
	Token             string
	Status            string // ISSUED | FROZEN | REDEEMED | EXPIRED
	Frozen            bool
	FreezeReason      string
	FrozenAt          string
	IssuedAt          string
	RedeemedAt        string
	RedeemedSegmentID string
}

type segmentRow struct {
	ID              string
	CredentialID    string
	AircraftID      string
	PermitID        string
	StartedAt       string
	EndedAt         string
	Track           string
	Deviation       bool
	DeviationDetail string
	Disposition     string
	RecordedAt      string
	RecordedBy      string
}

type conflictRow struct {
	ID          string
	Kind        string // PERMIT_PERMIT | PERMIT_ROUTE
	PermitA     string
	PermitB     string
	RouteVer    string
	Description string
	Points      string // JSON array of [lng,lat]
	CreatedAt   string
	ResolvedAt  string
}

// ---- request DTOs ----

type createPermitDTO struct {
	AircraftID           string          `json:"aircraftId"`
	MissionName          string          `json:"missionName"`
	Polygon              json.RawMessage `json:"polygon"`
	ValidFrom            string          `json:"validFrom"`
	ValidUntil           string          `json:"validUntil"`
	AltitudeMin          float64         `json:"altitudeMin"`
	AltitudeMax          float64         `json:"altitudeMax"`
	RequiredCapabilities []string        `json:"requiredCapabilities"`
}

type decisionDTO struct {
	Action  string          `json:"action"` // approve | shrink | deny | revoke
	Polygon json.RawMessage `json:"polygon"`
	Reason  string          `json:"reason"`
}

type routeDTO struct {
	Name     string          `json:"name"`
	Corridor json.RawMessage `json:"corridor"`
}

type redeemDTO struct {
	// No body fields strictly needed; Idempotency-Key header drives replays.
	Note string `json:"note"`
}

type heartbeatDTO struct {
	Status string `json:"status"` // ok | lost
}

type segmentDTO struct {
	CredentialID    string      `json:"credentialId"`
	StartedAt       string      `json:"startedAt"`
	EndedAt         string      `json:"endedAt"`
	Track           [][]float64 `json:"track"`
	Deviation       bool        `json:"deviation"`
	DeviationDetail string      `json:"deviationDetail"`
	Disposition     string      `json:"disposition"`
}

type ackDTO struct {
	// Optional; the Idempotency-Key header makes delayed/duplicate receipts safe.
	ReceivedAt string `json:"receivedAt"`
}
