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
	&RadiusCoATask{},
	&RadiusProfile{},
	&RadiusUser{},
	// Proxy subsystem
	&RadiusUpstream{},
	&RadiusRealmRoute{},
	// Portal V2 protocol subsystem
	&PortalNas{},
	// Accounting record / RFC 5080 dedup cache
	&RadiusAcctRecord{},
	&RadiusAcctDedup{},
	// Billing and account assets
	&BillingAccount{},
	&BillingAssetFlow{},
	&BillingPlan{},
	&BillingSubscription{},
	&BillingSessionUsage{},
	&BillingCharge{},
	&BillingOrder{},
	&BillingPayment{},
	&BillingCardBatch{},
	&BillingCard{},
	&BillingLedgerEntry{},
	&BillingRefund{},
}
