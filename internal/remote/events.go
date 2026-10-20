package remote

// The JSON Lines kinara-iroh writes on stdout (see sidecar/README.md). Only
// the fields the server uses or shows are listed; others are ignored.

type event struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	NodeID  string `json:"nodeId"`
	Ticket  string `json:"ticket"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Path    string `json:"path"`
}

// Status is the sidecar's last status line.
type Status struct {
	TS          int64       `json:"ts"`
	NodeID      string      `json:"nodeId"`
	Ticket      string      `json:"ticket"`
	UptimeSecs  int64       `json:"uptimeSecs"`
	Relay       RelayInfo   `json:"relay"`
	Addrs       []AddrInfo  `json:"addrs"`
	NetReport   *NetReport  `json:"netReport"`
	Warnings    []Warning   `json:"warnings"`
	Blocked     []string    `json:"blocked"`
	ActiveConns int64       `json:"activeStreams"`
	Peers       []PeerState `json:"peers"`
	// DirectSeen says when a device last used a direct path, by family.
	// Absent from sidecars older than this field.
	DirectSeen *DirectSeen `json:"directSeen,omitempty"`
}

// DirectSeen is how long ago a device was last on a direct IPv4 or IPv6
// path (nil: not since the sidecar started).
type DirectSeen struct {
	V4SecsAgo *int64 `json:"v4SecsAgo"`
	V6SecsAgo *int64 `json:"v6SecsAgo"`
}

type RelayInfo struct {
	Mode      string   `json:"mode"`
	URL       *string  `json:"url"`
	Connected bool     `json:"connected"`
	LatencyMs *float64 `json:"latencyMs"`
	LastError *string  `json:"lastError"`
}

type AddrInfo struct {
	Addr   string `json:"addr"`
	Family string `json:"family"`
	Scope  string `json:"scope"`
}

type NetReport struct {
	UDP                 bool    `json:"udp"`
	UDPv4               bool    `json:"udpV4"`
	UDPv6               bool    `json:"udpV6"`
	GlobalV4            *string `json:"globalV4"`
	GlobalV6            *string `json:"globalV6"`
	MappingVariesByDest *bool   `json:"mappingVariesByDest"`
	CaptivePortal       *bool   `json:"captivePortal"`
	PreferredRelay      *string `json:"preferredRelay"`
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PeerState struct {
	NodeID         string   `json:"nodeId"`
	Path           string   `json:"path"`
	RemoteAddr     *string  `json:"remoteAddr"`
	RelayURL       *string  `json:"relayUrl"`
	RTTMs          *float64 `json:"rttMs"`
	BytesIn        int64    `json:"bytesIn"`
	BytesOut       int64    `json:"bytesOut"`
	OpenStreams    int64    `json:"openStreams"`
	TotalStreams   int64    `json:"totalStreams"`
	ConnectedSince int64    `json:"connectedSince"`
}

// Note is a recent error or event from the sidecar, for the manager page.
type Note struct {
	At      int64  `json:"at"` // Unix seconds
	Code    string `json:"code"`
	Message string `json:"message"`
}
