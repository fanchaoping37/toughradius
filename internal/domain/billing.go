package domain

import "time"

// BillingAccount is the per-subscriber multi-asset ledger. Amounts are integer
// fen, time is seconds and traffic is bytes (SRS §10: no floats, no rounding
// drift). Version enables optimistic-lock debiting under concurrent accounting.
type BillingAccount struct {
	ID              int64     `json:"id,string" gorm:"primaryKey"`
	Username        string    `json:"username" gorm:"uniqueIndex;size:128"`
	RealName        string    `json:"real_name"`
	Status          string    `json:"status" gorm:"index;size:20"` // normal | suspended | frozen | closed
	CashFen         int64     `json:"cash_fen,string"`
	TimeSeconds     int64     `json:"time_seconds,string"`
	GiftTimeSeconds int64     `json:"gift_time_seconds,string"`
	BytesQuota      int64     `json:"bytes_quota,string"`
	GiftBytes       int64     `json:"gift_bytes,string"`
	FrozenFen       int64     `json:"frozen_fen,string"`
	Version         int64     `json:"version,string" gorm:"default:0"`
	ExpireAt        time.Time `json:"expire_at" gorm:"index"`
	Remark          string    `json:"remark"`
	CreatedAt       time.Time `json:"created_at" gorm:"index"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (BillingAccount) TableName() string { return "billing_account" }

// BillingAssetFlow is append-only. idempotent_key is unique so a retried
// accounting packet, payment callback or CoA event never credits/debits twice.
type BillingAssetFlow struct {
	ID            int64     `json:"id,string" gorm:"primaryKey"`
	AccountID     int64     `json:"account_id,string" gorm:"index"`
	AssetType     string    `json:"asset_type" gorm:"size:16"` // CASH | TIME | BYTES
	Direction     string    `json:"direction" gorm:"size:8"`   // IN | OUT
	Amount        int64     `json:"amount,string"`
	BalanceAfter  int64     `json:"balance_after,string"`
	BizType       string    `json:"biz_type" gorm:"size:40"` // recharge | charge | gift | refund | adjust
	RefID         string    `json:"ref_id" gorm:"index;size:128"`
	IdempotentKey string    `json:"idempotent_key" gorm:"uniqueIndex;size:200"`
	Operator      string    `json:"operator"`
	Remark        string    `json:"remark"`
	CreatedAt     time.Time `json:"created_at" gorm:"index"`
}

func (BillingAssetFlow) TableName() string { return "billing_asset_flow" }

// BillingPlan defines a product and its charging rules. StepsJSON/DiscountJSON
// carry tiered pricing and discount windows as JSON (SRS §10.2 t_price_rule).
type BillingPlan struct {
	ID           int64     `json:"id,string" gorm:"primaryKey"`
	Name         string    `json:"name" gorm:"size:100"`
	ChargeType   string    `json:"charge_type" gorm:"size:20"` // monthly | daily | time | traffic | tiered | capped | combo
	PriceFen     int64     `json:"price_fen,string"`
	RoundSeconds int       `json:"round_seconds"`
	RoundBytes   int64     `json:"round_bytes,string"`
	CapFen       int64     `json:"cap_fen,string"`
	QuotaSeconds int64     `json:"quota_seconds,string"`
	QuotaBytes   int64     `json:"quota_bytes,string"`
	UpBps        int64     `json:"up_bps,string"`
	DownBps      int64     `json:"down_bps,string"`
	MaxTerminals int       `json:"max_terminals"`
	ValidDays    int       `json:"valid_days"`
	StepsJSON    string    `json:"steps_json" gorm:"type:text"`
	DiscountJSON string    `json:"discount_json" gorm:"type:text"`
	Status       string    `json:"status" gorm:"size:20"`
	Remark       string    `json:"remark"`
	CreatedAt    time.Time `json:"created_at" gorm:"index"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (BillingPlan) TableName() string { return "billing_plan" }

// BillingSubscription ties an account to a plan for a period.
type BillingSubscription struct {
	ID            int64     `json:"id,string" gorm:"primaryKey"`
	AccountID     int64     `json:"account_id,string" gorm:"index"`
	PlanID        int64     `json:"plan_id,string" gorm:"index"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at" gorm:"index"`
	AutoRenew     bool      `json:"auto_renew"`
	Status        string    `json:"status" gorm:"size:20"` // active | expired | cancelled
	NextBillingAt time.Time `json:"next_billing_at"`
	Source        string    `json:"source" gorm:"size:20"`
	IdempotentKey string    `json:"idempotent_key" gorm:"uniqueIndex;size:200"`
	CreatedAt     time.Time `json:"created_at" gorm:"index"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (BillingSubscription) TableName() string { return "billing_subscription" }

// BillingSessionUsage tracks the last metered usage per RADIUS session so
// Interim-Update deltas are charged exactly once (SRS §12.4/§12.5).
type BillingSessionUsage struct {
	SessionID     string    `json:"session_id" gorm:"primaryKey;size:128"`
	AccountID     int64     `json:"account_id,string" gorm:"index"`
	Seconds       int64     `json:"seconds,string"`
	Bytes         int64     `json:"bytes,string"`
	ChargedFen    int64     `json:"charged_fen,string"`
	LastEventTime time.Time `json:"last_event_time"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (BillingSessionUsage) TableName() string { return "billing_session_usage" }

// BillingCharge is the issued bill line for a usage period or session.
type BillingCharge struct {
	ID          int64     `json:"id,string" gorm:"primaryKey"`
	AccountID   int64     `json:"account_id,string" gorm:"index"`
	SessionID   string    `json:"session_id" gorm:"index;size:128"`
	PlanID      int64     `json:"plan_id,string" gorm:"index"`
	BillNo      string    `json:"bill_no" gorm:"uniqueIndex;size:100"`
	Period      string    `json:"period" gorm:"index;size:20"`
	UsageAmount int64     `json:"usage_amount,string"`
	DiscountFen int64     `json:"discount_fen,string"`
	AmountFen   int64     `json:"amount_fen,string"`
	Capped      bool      `json:"capped"`
	Status      string    `json:"status" gorm:"size:20"` // issued | settled | overdue
	Remark      string    `json:"remark"`
	CreatedAt   time.Time `json:"created_at" gorm:"index"`
}

func (BillingCharge) TableName() string { return "billing_charge" }

// BillingOrder is a recharge/subscription order with idempotent creation.
type BillingOrder struct {
	OrderNo       string    `json:"order_no" gorm:"primaryKey;size:100"`
	AccountID     int64     `json:"account_id,string" gorm:"index"`
	PlanID        int64     `json:"plan_id,string"`
	AmountFen     int64     `json:"amount_fen,string"`
	Status        string    `json:"status" gorm:"index;size:20"` // pending | paid | cancelled | refunded
	Channel       string    `json:"channel" gorm:"size:20"`
	IdempotentKey string    `json:"idempotent_key" gorm:"uniqueIndex;size:200"`
	PaidAt        time.Time `json:"paid_at"`
	ExpiredAt     time.Time `json:"expired_at"`
	CreatedAt     time.Time `json:"created_at" gorm:"index"`
}

func (BillingOrder) TableName() string { return "billing_order" }

// BillingPayment stores raw payment-channel callbacks for reconciliation.
type BillingPayment struct {
	PaymentNo    string    `json:"payment_no" gorm:"primaryKey;size:100"`
	OrderNo      string    `json:"order_no" gorm:"index;size:100"`
	Channel      string    `json:"channel" gorm:"size:20"`
	ChannelTxnID string    `json:"channel_txn_id" gorm:"uniqueIndex;size:150"`
	AmountFen    int64     `json:"amount_fen,string"`
	RawNotify    string    `json:"raw_notify" gorm:"type:text"`
	SignVerified bool      `json:"sign_verified"`
	Status       string    `json:"status" gorm:"size:20"`
	NotifiedAt   time.Time `json:"notified_at"`
	CreatedAt    time.Time `json:"created_at" gorm:"index"`
}

func (BillingPayment) TableName() string { return "billing_payment" }

// BillingCardBatch / BillingCard implement prepaid card issuing and redemption.
type BillingCardBatch struct {
	BatchNo      string    `json:"batch_no" gorm:"primaryKey;size:100"`
	FaceValueFen int64     `json:"face_value_fen,string"`
	AssetType    string    `json:"asset_type" gorm:"size:16"`
	TotalQty     int       `json:"total_qty"`
	ValidUntil   time.Time `json:"valid_until"`
	CreatedAt    time.Time `json:"created_at" gorm:"index"`
}

func (BillingCardBatch) TableName() string { return "billing_card_batch" }

type BillingCard struct {
	CardNo   string    `json:"card_no" gorm:"primaryKey;size:100"`
	BatchNo  string    `json:"batch_no" gorm:"index;size:100"`
	CodeHash string    `json:"code_hash"`
	Status   string    `json:"status" gorm:"index;size:20"` // unused | active | used | void
	UsedBy   int64     `json:"used_by,string" gorm:"index"`
	UsedAt   time.Time `json:"used_at"`
	ExpireAt time.Time `json:"expire_at"`
}

func (BillingCard) TableName() string { return "billing_card" }
