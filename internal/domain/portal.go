package domain

import "time"

// PortalNas is a NAS device that speaks the Huawei / CMCC Portal V2 protocol.
// The shared secret is used to sign/verify Portal authenticators; the device
// itself sends RADIUS Access-Request packets to ToughRADIUS on UDP 1812.
type PortalNas struct {
	ID        int64     `json:"id,string" form:"id"`
	NodeId    int64     `json:"node_id,string" form:"node_id"`
	Name      string    `json:"name" form:"name"`
	Addr      string    `json:"addr" form:"addr"`                  // device IP or hostname
	Port      int       `json:"port" form:"port"`                  // device Portal port (default 2000)
	Secret    string    `json:"secret" form:"secret"`              // Portal shared secret
	Status    string    `gorm:"index" json:"status" form:"status"` // enabled | disabled
	Remark    string    `json:"remark" form:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName specifies the Portal NAS table name.
func (PortalNas) TableName() string { return "portal_nas" }
