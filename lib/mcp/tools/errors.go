package tools

import (
	"errors"
	"fmt"
)

// ErrNoTenant is returned when a request reaches a handler without the
// auth middleware having attached a TenantContext. This is an internal
// error — a misconfigured server, not user error — so we keep the message
// generic to avoid leaking implementation detail.
var ErrNoTenant = errors.New("internal: missing tenant context")

// userErrf wraps a sentinel cause with a user-visible message. Callers
// should prefer this over raw fmt.Errorf for errors that surface to the
// MCP client.
func userErrf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
