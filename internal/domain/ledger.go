package domain

import "time"

// BillingLedgerEntry 是财务双分录账本的一行。金额以分为单位，
// DEBIT 与 CREDIT 必须成对出现且合计相等；EntryNo 唯一保证重复入账幂等。
type BillingLedgerEntry struct {
	ID          int64     `json:"id,string" gorm:"primaryKey"`
	EntryNo     string    `json:"entry_no" gorm:"uniqueIndex;size:128"`
	BizType     string    `json:"biz_type" gorm:"size:40"`
	RefID       string    `json:"ref_id" gorm:"index;size:128"`
	AccountID   int64     `json:"account_id,string" gorm:"index"`
	Direction   string    `json:"direction" gorm:"size:8"` // DEBIT | CREDIT
	AccountCode string    `json:"account_code" gorm:"index;size:40"`
	AmountFen   int64     `json:"amount_fen,string"`
	Currency    string    `json:"currency" gorm:"size:8"`
	Summary     string    `json:"summary"`
	Operator    string    `json:"operator"`
	CreatedAt   time.Time `json:"created_at" gorm:"index"`
}

func (BillingLedgerEntry) TableName() string { return "billing_ledger_entry" }

// BillingRefund 记录一次订单退款。IdempotentKey 唯一，重复回调不会发生二次退款。
// Status 取值为 pending | succeeded | failed。
type BillingRefund struct {
	ID            int64     `json:"id,string" gorm:"primaryKey"`
	RefundNo      string    `json:"refund_no" gorm:"uniqueIndex;size:100"`
	OrderNo       string    `json:"order_no" gorm:"index;size:100"`
	PaymentNo     string    `json:"payment_no" gorm:"size:100"`
	AmountFen     int64     `json:"amount_fen,string"`
	Reason        string    `json:"reason"`
	Status        string    `json:"status" gorm:"size:20"`
	IdempotentKey string    `json:"idempotent_key" gorm:"uniqueIndex;size:200"`
	Operator      string    `json:"operator"`
	CreatedAt     time.Time `json:"created_at" gorm:"index"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (BillingRefund) TableName() string { return "billing_refund" }
