package egress

// VM network egress modes.
const (
	ModeAllowlist = "allowlist" // restricted network; only exit is the allowlist proxy
	ModeOpen      = "open"      // unfiltered user-mode networking
	ModeNone      = "none"      // restricted network, no exit
)
