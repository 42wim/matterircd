package matrixclient

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// ErrVerificationUnavailable means encryption or a VerificationHandler/provider
// was not enabled when the client was created.
var ErrVerificationUnavailable = errors.New("matrixclient: interactive verification is not enabled")

// VerificationStage describes a user-visible step of interactive verification.
type VerificationStage string

const (
	VerificationRequested VerificationStage = "requested"
	VerificationReady     VerificationStage = "ready"
	VerificationSAS       VerificationStage = "sas"
	VerificationCancelled VerificationStage = "canceled"
	VerificationDone      VerificationStage = "done"
)

// Verification is a verification update. Correlate updates by TransactionID.
// UserID is set on requests; DeviceID is set on requests and ready updates.
type Verification struct {
	TransactionID id.VerificationTransactionID
	Stage         VerificationStage
	UserID        id.UserID
	DeviceID      id.DeviceID
	SupportsSAS   bool // ready updates

	// SAS updates contain emojis with matching descriptions, decimals, or both.
	Emojis            []rune
	EmojiDescriptions []string
	Decimals          []int

	CancelCode event.VerificationCancelCode
	Reason     string
	Method     event.VerificationMethod // done updates
}

// VerificationHandler optionally receives interactive verification updates.
// Callbacks may run concurrently, including before Ready and from API calls.
// They must not block or call verification methods: queue updates for a separate
// consumer, as the verification engine may hold its transaction lock.
type VerificationHandler interface {
	OnVerification(v *Verification)
}

// VerificationController is the SAS verification engine supplied by a Crypto.
type VerificationController interface {
	StartVerification(ctx context.Context, userID id.UserID) (id.VerificationTransactionID, error)
	AcceptVerification(ctx context.Context, txnID id.VerificationTransactionID) error
	StartSAS(ctx context.Context, txnID id.VerificationTransactionID) error
	ConfirmSAS(ctx context.Context, txnID id.VerificationTransactionID) error
	CancelVerification(ctx context.Context, txnID id.VerificationTransactionID, code event.VerificationCancelCode, reason string) error
}

// VerificationProvider is optionally implemented by a Crypto. InitVerification
// is called after Crypto.Init, before syncing, only with a VerificationHandler.
type VerificationProvider interface {
	InitVerification(ctx context.Context, notify func(*Verification)) (VerificationController, error)
}

// StartVerification requests SAS verification from a user's other devices.
// For self-verification pass Client.UserID(). The first device accepting wins;
// this does not target a single selected device.
func (c *Client) StartVerification(ctx context.Context, userID id.UserID) (id.VerificationTransactionID, error) {
	if c.verification == nil {
		return "", ErrVerificationUnavailable
	}

	if userID == "" {
		return "", errors.New("matrixclient: verification requires a user ID")
	}

	return c.verification.StartVerification(ctx, userID)
}

// AcceptVerification accepts an incoming request, but does not trust the device.
func (c *Client) AcceptVerification(ctx context.Context, txnID id.VerificationTransactionID) error {
	if c.verification == nil {
		return ErrVerificationUnavailable
	}

	return c.verification.AcceptVerification(ctx, txnID)
}

// StartSAS begins the key exchange after VerificationReady. Only one side needs
// to call it; incoming SAS starts are handled automatically.
func (c *Client) StartSAS(ctx context.Context, txnID id.VerificationTransactionID) error {
	if c.verification == nil {
		return ErrVerificationUnavailable
	}

	return c.verification.StartSAS(ctx, txnID)
}

// ConfirmSAS confirms that the user compared the SAS on both devices and it
// matches. Wait for VerificationDone before reporting success.
func (c *Client) ConfirmSAS(ctx context.Context, txnID id.VerificationTransactionID) error {
	if c.verification == nil {
		return ErrVerificationUnavailable
	}

	return c.verification.ConfirmSAS(ctx, txnID)
}

// CancelVerification rejects or cancels a transaction. Use
// event.VerificationCancelCodeSASMismatch when the displayed SAS does not match.
// A successful local cancellation is reported by the return value, not a callback.
func (c *Client) CancelVerification(ctx context.Context, txnID id.VerificationTransactionID, code event.VerificationCancelCode, reason string) error {
	if c.verification == nil {
		return ErrVerificationUnavailable
	}

	return c.verification.CancelVerification(ctx, txnID, code, reason)
}
