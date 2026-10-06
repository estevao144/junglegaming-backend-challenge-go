package domain

import (
	"strings"
	"time"
)

// IDs are opaque, nonblank strings without surrounding whitespace.
// Generating UUIDs and validating transport-specific formats belong to adapters.
func validID(id string) bool { return id != "" && strings.TrimSpace(id) == id }

func validTimes(createdAt, updatedAt time.Time) bool {
	return !createdAt.IsZero() && !updatedAt.IsZero() && !updatedAt.Before(createdAt)
}
