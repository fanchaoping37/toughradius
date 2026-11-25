package domain

import "time"

// RadiusCoATask is the durable outbox entry for an RFC 5176 CoA/Disconnect
// exchange. The task is written before transmission so a process restart can
// retry it with the same Identifier (SRS §12.5, item 4).
type RadiusCoATask struct {
	ID             int64  `json:"id,string" gorm:"primaryKey"`
	SessionID      int64  `json:"session_id,string" gorm:"index"`
	AcctSessionID  string `json:"acct_session_id" gorm:"index;size:128"`
	Username       string `json:"username" gorm:"index;size:128"`
	NasAddr        string `json:"nas_addr" gorm:"index;size:100"`
	NasPort        int    `json:"nas_port"`
	Action         string `json:"action" gorm:"index;size:20"` // coa | disconnect
	AttributesJSON string `json:"attributes_json" gorm:"type:text"`
	// IdentityJSON stores RFC 5176 session-identification attributes so a
	// pending task can be retried even after the online row disappears.
	IdentityJSON          string    `json:"identity_json" gorm:"type:text"`
	FilterID              string    `json:"filter_id" gorm:"size:128"`
	SessionTimeoutSeconds int       `json:"session_timeout_seconds"`
	Identifier            int       `json:"identifier"`
	Status                string    `json:"status" gorm:"index;size:20"` // pending | sending | ack | nak | timeout | failed
	Attempts              int       `json:"attempts"`
	ErrorCause            int       `json:"error_cause"`
	ErrorCauseText        string    `json:"error_cause_text"`
	Error                 string    `json:"error"`
	RTTMillis             int64     `gorm:"column:rtt_ms" json:"rtt_ms"`
	OperatorID            int64     `json:"operator_id,string" gorm:"index"`
	OperatorName          string    `json:"operator_name"`
	Reason                string    `json:"reason"`
	CreatedAt             time.Time `json:"created_at" gorm:"index"`
	SentAt                time.Time `json:"sent_at"`
	FinishedAt            time.Time `json:"finished_at"`
}

func (RadiusCoATask) TableName() string { return "radius_coa_task" }
