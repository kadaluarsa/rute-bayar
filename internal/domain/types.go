package domain

import (
	"encoding/json"
	"time"
)

type ProviderCode string

const (
	ProviderMidtrans ProviderCode = "midtrans"
	ProviderXendit   ProviderCode = "xendit"
	ProviderDoku     ProviderCode = "doku"
	ProviderIPaymu   ProviderCode = "ipaymu"
)

func SupportedProviders() []ProviderCode {
	return []ProviderCode{ProviderMidtrans, ProviderXendit, ProviderDoku, ProviderIPaymu}
}

type Environment string

const (
	EnvironmentSandbox    Environment = "sandbox"
	EnvironmentProduction Environment = "production"
)

type PaymentStatus string

const (
	PaymentStatusPending         PaymentStatus = "pending"
	PaymentStatusPaid            PaymentStatus = "paid"
	PaymentStatusFailed          PaymentStatus = "failed"
	PaymentStatusExpired         PaymentStatus = "expired"
	PaymentStatusCancelled       PaymentStatus = "cancelled"
	PaymentStatusRefunded        PaymentStatus = "refunded"
	PaymentStatusPartialRefunded PaymentStatus = "partial_refunded"
	PaymentStatusSettled         PaymentStatus = "settled"
	PaymentStatusAuthorized      PaymentStatus = "authorized"
	PaymentStatusCaptured        PaymentStatus = "captured"
)

type ProviderAccount struct {
	ID             string
	ProviderCode   ProviderCode
	Environment    Environment
	DisplayName    string
	CredentialJSON json.RawMessage
	ConfigJSON     json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type PaymentIntent struct {
	ID           string
	ExternalRef  string
	ProviderCode ProviderCode
	Amount       int64
	Currency     string
	Status       PaymentStatus
	MetadataJSON json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type PaymentAttempt struct {
	ID                string
	PaymentIntentID   string
	ProviderCode      ProviderCode
	RequestJSON       json.RawMessage
	ResponseJSON      json.RawMessage
	Status            PaymentStatus
	ProviderReference string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type PaymentStatusCheck struct {
	ID                string
	PaymentIntentID   string
	ProviderCode      ProviderCode
	RequestJSON       json.RawMessage
	ResponseJSON      json.RawMessage
	Status            PaymentStatus
	ProviderReference string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type WebhookEvent struct {
	ID               string
	ProviderCode     ProviderCode
	ProviderEventID  string
	EventType        string
	SignatureValid   bool
	PayloadJSON      json.RawMessage
	HeadersJSON      json.RawMessage
	ReceivedAt       time.Time
	ProcessedAt      *time.Time
	ProcessingStatus string
}

type Refund struct {
	ID                string
	PaymentIntentID   string
	ProviderCode      ProviderCode
	Amount            int64
	Status            PaymentStatus
	RequestJSON       json.RawMessage
	ResponseJSON      json.RawMessage
	ProviderReference string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RouteExecutionStatus is the outcome of executing a routing token.
type RouteExecutionStatus string

const (
	// RouteExecutionInProgress marks an execution that holds the per-order lock.
	RouteExecutionInProgress RouteExecutionStatus = "in_progress"
	// RouteExecutionIssued means a gateway accepted the payment and returned
	// an instrument (VA number, QR string, or hosted page).
	RouteExecutionIssued RouteExecutionStatus = "issued"
	// RouteExecutionFailed means every gateway definitively refused; no
	// gateway created a payment. A new routing token may retry.
	RouteExecutionFailed RouteExecutionStatus = "failed"
	// RouteExecutionRequiresReconcile means a gateway may have created the
	// payment but did not confirm it. Never retry blindly: reconcile first.
	RouteExecutionRequiresReconcile RouteExecutionStatus = "requires_reconcile"
	// RouteExecutionRejected means the mandate check (EX-3) refused the
	// charge before any gateway was called.
	RouteExecutionRejected RouteExecutionStatus = "mandate_rejected"
)

// RouteExecution is the per-order idempotency record for payment execution.
type RouteExecution struct {
	OrderID      string
	TokenID      string
	Environment  Environment
	Method       string
	Amount       int64
	Currency     string
	Status       RouteExecutionStatus
	ProviderCode ProviderCode
	ResultJSON   json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// OutboundEventStatus tracks delivery of a signed event to Wishpull.
type OutboundEventStatus string

const (
	OutboundEventPending   OutboundEventStatus = "pending"
	OutboundEventDelivered OutboundEventStatus = "delivered"
	OutboundEventDead      OutboundEventStatus = "dead"
)

// OutboundEvent is one row of the transactional outbox that carries
// normalized payment events to Wishpull's webhook ingress.
type OutboundEvent struct {
	ID             string
	EventType      string
	OrderID        string
	ProviderCode   ProviderCode
	WebhookEventID string
	PayloadJSON    json.RawMessage
	Status         OutboundEventStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeliveredAt    *time.Time
}
