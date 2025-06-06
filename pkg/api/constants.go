package api

// VM Types
const (
	VMTypeQemu = "qemu"
	VMTypeLXC  = "lxc"
)

// VM Status
const (
	VMStatusRunning = "running"
	VMStatusStopped = "stopped"
)

// IP Types
const (
	IPTypeIPv4 = "ipv4"
	IPTypeIPv6 = "ipv6"
)

// Common strings
const (
	StringTrue = "true"
	StringNA   = "N/A"
)

// Network interface names
const (
	LoopbackInterface = "lo"
)

// Node types
const (
	NodeType = "node"
)

// UI Pages
const (
	PageNodes  = "Nodes"
	PageGuests = "Guests"
)

// Menu actions
const (
	ActionRefresh   = "Refresh"
	ActionOpenShell = "Open Shell"
)
