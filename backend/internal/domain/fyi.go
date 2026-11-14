package domain

import (
	"errors"
	"strings"
	"time"
)

// MaxFYIMessageBytes bounds one `ao send --fyi` message, matching the
// /sessions/{id}/send message limit.
const MaxFYIMessageBytes = 4096

// ErrInvalidFYIMessage reports an invalid FYI message or delivery transition input.
var ErrInvalidFYIMessage = errors.New("invalid fyi message")

// FYIMessage is one informational message held for a session's next turn.
// Storing it never starts a turn: AO appends every pending FYI, in arrival
// order, to whatever turn the target starts next, and acknowledges it only once
// that turn is durably accepted. It shares the report outbox's delivery states.
type FYIMessage struct {
	// ID is the durable arrival sequence; a higher ID arrived later.
	ID              int64
	TargetSessionID SessionID
	// SenderSessionID is cooperative attribution, empty for the user.
	SenderSessionID SessionID
	Message         string
	CreatedAt       time.Time
	DeliveryState   ReportDeliveryState
	ClaimToken      string
	ClaimedAt       time.Time
	AcknowledgedAt  time.Time
	// SenderDisplayName and SenderProjectID are resolved at delivery time for
	// presentation and are not persisted with the row.
	SenderDisplayName string
	SenderProjectID   ProjectID
}

// Validate checks the content invariants of a new FYI message.
func (m FYIMessage) Validate() error {
	if m.TargetSessionID == "" || m.CreatedAt.IsZero() || strings.TrimSpace(m.Message) == "" || len(m.Message) > MaxFYIMessageBytes {
		return ErrInvalidFYIMessage
	}
	return nil
}
