package domain

var Tables = []interface{}{
	// System
	&SysConfig{},
	&SysOpr{},
	&SysOprLog{},
	&SysCert{},
	// Network
	&NetNode{},
	&NetNas{},
	// Radius
	&RadiusAccounting{},
	&RadiusOnline{},
	&RadiusSessionActionAudit{},
	&RadiusProfile{},
	&RadiusUser{},
	// Proxy subsystem
	&RadiusUpstream{},
	&RadiusRealmRoute{},
	// Portal V2 protocol subsystem
	&PortalNas{},
}
