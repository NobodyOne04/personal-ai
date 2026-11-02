package ports

import "time"

// Clock supplies the current time, so time-dependent rules are testable.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces unique identifiers.
type IDGenerator interface {
	NewID() string
}
