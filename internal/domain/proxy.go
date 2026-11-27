package domain

import "time"

// RADIUS proxy subsystem models: upstream servers, realm routing rules.
//
// The proxy subsystem enables three deployment topologies:
//
//  1. Replication ("抄送"): role=replicate upstreams receive a fire-and-forget
//     copy of every Access-Request / Accounting-Request so a peer server can
//     maintain its own user/session database.
//  2. Cluster load balancing: a realm route with strategy round_robin/weight
//     distributes proxied authentication across several backend RADIUS nodes.
//  3. Roaming / multi-level ISP proxy: username@realm traffic is forwarded to
//     the upstream owning that realm, optionally rewriting the credentials
//     (campus account → carrier account).

// Upstream role: proxy requires a response, replicate is fire-and-forget.
const (
	UpstreamRoleProxy     = "proxy"
	UpstreamRoleReplicate = "replicate"
)

// Realm route upstream selection strategies.
const (
	RealmStrategySingle     = "single"
	RealmStrategyRoundRobin = "round_robin"
	RealmStrategyWeight     = "weight"
)

// Realm route accounting forwarding modes.
const (
	AcctModeBoth    = "both"    // store locally and forward upstream
	AcctModeForward = "forward" // forward upstream only
	AcctModeLocal   = "local"   // store locally only
)

// RadiusUpstream is a remote RADIUS server that this instance can forward
// authentication/accounting packets to, or replicate request copies to.
type RadiusUpstream struct {
	ID       int64  `json:"id,string" form:"id"`
	NodeId   int64  `json:"node_id,string" form:"node_id"`
	Name     string `json:"name" form:"name"`
	Addr     string `json:"addr" form:"addr"`           // Upstream IP or hostname
	AuthPort int    `json:"auth_port" form:"auth_port"` // Upstream auth port (default 1812)
	AcctPort int    `json:"acct_port" form:"acct_port"` // Upstream acct port (default 1813)
	CoaPort  int    `json:"coa_port" form:"coa_port"`   // Upstream dynamic-authorization port (default 3799)
	Secret   string `json:"secret" form:"secret"`
	// Role selects the forwarding behavior: "proxy" (await upstream response
	// and relay it back to the NAS) or "replicate" (send a copy and ignore the
	// response). See UpstreamRoleProxy / UpstreamRoleReplicate.
	Role string `gorm:"index" json:"role" form:"role"`
	// Weight is used by the "weight" realm strategy; zero is treated as 1.
	Weight    int       `json:"weight" form:"weight"`
	Status    string    `gorm:"index" json:"status" form:"status"` // enabled | disabled
	Remark    string    `json:"remark" form:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName specifies the upstream server table name.
func (RadiusUpstream) TableName() string {
	return "radius_upstream"
}

// RadiusRealmRoute maps a username realm (the suffix after the last "@") to a
// set of upstream servers. The special realm "*" is the catch-all route used
// for cluster load balancing of realm-less usernames.
type RadiusRealmRoute struct {
	ID     int64 `json:"id,string" form:"id"`
	NodeId int64 `json:"node_id,string" form:"node_id"`
	// Realm is matched case-insensitively against the username suffix after
	// the last "@". "*" matches usernames without a route-specific realm.
	Realm string `gorm:"index" json:"realm" form:"realm"`
	// Strategy selects the upstream pool algorithm:
	// single / round_robin / weight. See RealmStrategy* constants.
	Strategy string `json:"strategy" form:"strategy"`
	// UpstreamIds is a comma-separated list of RadiusUpstream IDs that form
	// the route's upstream pool. Kept denormalized to avoid a join table.
	UpstreamIds string `json:"upstream_ids" form:"upstream_ids"`
	// Credential transformation (campus account → carrier account):
	// RewrittenUsername = UserPrefix + local-part + UserSuffix, and when
	// MappedPassword is non-empty the PAP password is replaced entirely.
	// Empty values pass the original credentials through.
	UserPrefix     string `json:"user_prefix" form:"user_prefix"`
	UserSuffix     string `json:"user_suffix" form:"user_suffix"`
	MappedPassword string `json:"mapped_password" form:"mapped_password"`
	// AcctMode controls accounting handling for proxied sessions:
	// both (default) / forward / local. See AcctMode* constants.
	AcctMode string `json:"acct_mode" form:"acct_mode"`
	// Dial-proxy adapter fields. Empty DialVendor means an ordinary proxy
	// route with no vendor-specific upstream request rewriting. DialVendor
	// accepts ruijie/drcom/panabit/ctcc/cmcc/cucc/xujing. DialFilterID
	// overrides the vendor default Filter-Id. DialVendorAttrsJSON is a JSON
	// array of extra RADIUS attributes (raw wire values in hex) injected into
	// the upstream request; it is validated before any packet is sent.
	DialVendor          string    `json:"dial_vendor" form:"dial_vendor"`
	DialFilterID        string    `json:"dial_filter_id" form:"dial_filter_id"`
	DialVendorAttrsJSON string    `json:"dial_vendor_attrs_json" form:"dial_vendor_attrs_json"`
	Status              string    `gorm:"index" json:"status" form:"status"` // enabled | disabled
	Remark              string    `json:"remark" form:"remark"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// TableName specifies the realm route table name.
func (RadiusRealmRoute) TableName() string {
	return "radius_realm_route"
}
