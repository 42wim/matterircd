package e2ee

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matterbridge/matrixclient"
)

// InitVerification implements matrixclient.VerificationProvider.
func (h helper) InitVerification(ctx context.Context, notify func(*matrixclient.Verification)) (matrixclient.VerificationController, error) {
	callbacks := verificationCallbacks{ctx: ctx, notify: notify}
	store := &lockedStore{VerificationStore: verificationhelper.NewInMemoryVerificationStore(), deleted: map[id.VerificationTransactionID]time.Time{}}
	vh := verificationhelper.NewVerificationHelper(h.client, h.Machine(), store, callbacks, false, false, true)

	// The helper registers its handlers on the client's syncer during Init.
	syncer, ok := h.client.Syncer.(mautrix.ExtensibleSyncer)
	if !ok {
		return nil, errors.New("e2ee: client syncer must implement ExtensibleSyncer")
	}

	h.client.Syncer = &relatedSyncer{ExtensibleSyncer: syncer, Syncer: h.client.Syncer, client: h.client, store: store}

	err := vh.Init(ctx)
	h.client.Syncer = relatedOriginal(h.client.Syncer)

	if err != nil {
		return nil, err
	}

	h.client.Verification = vh

	return &roomHelper{VerificationHelper: vh, client: h.client, store: store, callbacks: callbacks, session: ctx}, nil
}

// lockedStore serializes access to the helper's in-memory store, which is
// otherwise only protected by the helper's private lock.
type lockedStore struct {
	verificationhelper.VerificationStore

	mu sync.Mutex
	// op serializes whole verification operations (forwarded events and
	// expiry): mautrix drops its lock between lookup and handler, so per-call
	// locking alone allows a reply to resurrect an expired transaction.
	op sync.Mutex

	// deleted holds recently removed IDs. Mautrix's own expiry and cancellation
	// delete without taking op, so a handler that looked the transaction up
	// earlier must not be able to save it back.
	deleted map[id.VerificationTransactionID]time.Time
}

const tombstoneTTL = time.Minute

func (s *lockedStore) DeleteVerification(ctx context.Context, txnID id.VerificationTransactionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for k, at := range s.deleted {
		if now.Sub(at) > tombstoneTTL {
			delete(s.deleted, k)
		}
	}

	s.deleted[txnID] = now

	return s.VerificationStore.DeleteVerification(ctx, txnID)
}

func (s *lockedStore) GetVerificationTransaction(ctx context.Context, txnID id.VerificationTransactionID) (verificationhelper.VerificationTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.VerificationStore.GetVerificationTransaction(ctx, txnID)
}

func (s *lockedStore) SaveVerificationTransaction(ctx context.Context, txn verificationhelper.VerificationTransaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, gone := s.deleted[txn.TransactionID]; gone {
		return verificationhelper.ErrUnknownVerificationTransaction
	}

	return s.VerificationStore.SaveVerificationTransaction(ctx, txn)
}

func (s *lockedStore) FindVerificationTransactionForUserDevice(ctx context.Context, userID id.UserID, deviceID id.DeviceID) (verificationhelper.VerificationTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.VerificationStore.FindVerificationTransactionForUserDevice(ctx, userID, deviceID)
}

func (s *lockedStore) GetAllVerificationTransactions(ctx context.Context) ([]verificationhelper.VerificationTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.VerificationStore.GetAllVerificationTransactions(ctx)
}

// relatedSyncer makes the helper's in-room handlers see the request event ID
// from m.relates_to as the event ID: replies are filed under the request, not
// under their own event ID. Only events from the transaction's counterpart in
// its room are forwarded, which also drops this device's own sync echoes.
type relatedSyncer struct {
	mautrix.ExtensibleSyncer
	mautrix.Syncer

	client *mautrix.Client
	store  *lockedStore
}

func relatedOriginal(s mautrix.Syncer) mautrix.Syncer {
	if r, ok := s.(*relatedSyncer); ok {
		return r.Syncer
	}

	return s
}

func (r *relatedSyncer) OnEventType(eventType event.Type, callback mautrix.EventHandler) {
	switch eventType {
	case event.InRoomVerificationReady, event.InRoomVerificationStart, event.InRoomVerificationDone,
		event.InRoomVerificationCancel, event.InRoomVerificationAccept, event.InRoomVerificationKey,
		event.InRoomVerificationMAC:
		inner := callback
		callback = func(ctx context.Context, evt *event.Event) {
			rel, ok := evt.Content.Parsed.(event.Relatable)
			if !ok || rel.OptionalGetRelatesTo() == nil || rel.OptionalGetRelatesTo().EventID == "" {
				return
			}

			r.store.op.Lock()
			defer r.store.op.Unlock()

			txn, err := r.store.GetVerificationTransaction(ctx, id.VerificationTransactionID(rel.OptionalGetRelatesTo().EventID))
			if err != nil || evt.RoomID != txn.RoomID || evt.Sender != txn.TheirUserID {
				return
			}

			// Unsigned.TransactionID is only set on events sent by this client.
			if evt.Unsigned.TransactionID != "" {
				return
			}

			// Self-verification: the counterpart shares our user ID, so tell this device apart.
			if fromDevice, _ := evt.Content.Raw["from_device"].(string); evt.Sender == r.client.UserID && id.DeviceID(fromDevice) == r.client.DeviceID {
				return
			}

			related := *evt
			related.ID = rel.OptionalGetRelatesTo().EventID
			inner(ctx, &related)
		}
	}

	r.ExtensibleSyncer.OnEventType(eventType, callback)
}

// roomHelper replaces the helper's in-room start, which encrypts the request
// itself and then has SendMessageEvent encrypt it again, and never expires it.
type roomHelper struct {
	*verificationhelper.VerificationHelper

	client    *mautrix.Client
	store     *lockedStore
	callbacks verificationCallbacks
	session   context.Context //nolint:containedctx
}

// CancelVerification serializes with forwarded in-room event handlers.
func (r *roomHelper) CancelVerification(ctx context.Context, txnID id.VerificationTransactionID, code event.VerificationCancelCode, reason string) error {
	r.store.op.Lock()
	defer r.store.op.Unlock()

	return r.VerificationHelper.CancelVerification(ctx, txnID, code, reason)
}

// DismissVerification serializes with forwarded in-room event handlers.
func (r *roomHelper) DismissVerification(ctx context.Context, txnID id.VerificationTransactionID) error {
	r.store.op.Lock()
	defer r.store.op.Unlock()

	return r.VerificationHelper.DismissVerification(ctx, txnID)
}

const verificationTimeout = 10 * time.Minute

func (r *roomHelper) StartInRoomVerification(ctx context.Context, roomID id.RoomID, to id.UserID) (id.VerificationTransactionID, error) {
	content := event.MessageEventContent{
		MsgType:    event.MsgVerificationRequest,
		Body:       fmt.Sprintf("%s is requesting to verify your device, but your client does not support verification, so you may need to use a different verification method.", r.client.UserID),
		FromDevice: r.client.DeviceID,
		Methods:    []event.VerificationMethod{event.VerificationMethodSAS},
		To:         to,
	}

	// Held until the transaction is saved so a fast reply is not dropped as unknown.
	r.store.op.Lock()
	defer r.store.op.Unlock()

	resp, err := r.client.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	if err != nil {
		return "", fmt.Errorf("failed to send verification request: %w", err)
	}

	txnID := id.VerificationTransactionID(resp.EventID)
	expires := time.Now().Add(verificationTimeout)

	err = r.store.SaveVerificationTransaction(ctx, verificationhelper.VerificationTransaction{
		ExpirationTime:    jsontime.UnixMilli{Time: expires},
		RoomID:            roomID,
		VerificationState: verificationhelper.VerificationStateRequested,
		TransactionID:     txnID,
		TheirUserID:       to,
	})
	if err != nil {
		return "", err
	}

	// The session context outlives this call but ends on logout, when the
	// crypto store is closed.
	time.AfterFunc(time.Until(expires), func() { r.expire(r.session, txnID) })

	return txnID, nil
}

// expire drops a request that is still pending. The transaction is dismissed
// even if the cancellation cannot be delivered, and consumers are notified
// only when this call removed it.
func (r *roomHelper) expire(ctx context.Context, txnID id.VerificationTransactionID) {
	if ctx.Err() != nil {
		return
	}

	r.store.op.Lock()
	defer r.store.op.Unlock()

	_, err := r.store.GetVerificationTransaction(ctx, txnID)
	if err != nil {
		return
	}

	const reason = "verification timed out"

	err = r.VerificationHelper.CancelVerification(ctx, txnID, event.VerificationCancelCodeTimeout, reason)
	if err != nil {
		r.client.Log.Warn().Err(err).Stringer("transaction_id", txnID).Msg("Failed to cancel expired verification")

		err = r.VerificationHelper.DismissVerification(ctx, txnID)
		if err != nil {
			return
		}
	}

	r.callbacks.send(&matrixclient.Verification{
		TransactionID: txnID,
		Stage:         matrixclient.VerificationCancelled,
		CancelCode:    event.VerificationCancelCodeTimeout,
		Reason:        reason,
	})
}

type verificationCallbacks struct {
	ctx    context.Context //nolint:containedctx
	notify func(*matrixclient.Verification)
}

func (v verificationCallbacks) send(update *matrixclient.Verification) {
	if v.ctx.Err() == nil {
		v.notify(update)
	}
}

func (v verificationCallbacks) VerificationRequested(_ context.Context, txnID id.VerificationTransactionID, userID id.UserID, deviceID id.DeviceID) {
	v.send(&matrixclient.Verification{
		TransactionID: txnID,
		Stage:         matrixclient.VerificationRequested,
		UserID:        userID,
		DeviceID:      deviceID,
	})
}

func (v verificationCallbacks) VerificationReady(_ context.Context, txnID id.VerificationTransactionID, deviceID id.DeviceID, supportsSAS, _ bool, _ *verificationhelper.QRCode) {
	v.send(&matrixclient.Verification{
		TransactionID: txnID,
		Stage:         matrixclient.VerificationReady,
		DeviceID:      deviceID,
		SupportsSAS:   supportsSAS,
	})
}

func (v verificationCallbacks) ShowSAS(_ context.Context, txnID id.VerificationTransactionID, emojis []rune, descriptions []string, decimals []int) {
	v.send(&matrixclient.Verification{
		TransactionID:     txnID,
		Stage:             matrixclient.VerificationSAS,
		Emojis:            slices.Clone(emojis),
		EmojiDescriptions: slices.Clone(descriptions),
		Decimals:          slices.Clone(decimals),
	})
}

func (v verificationCallbacks) VerificationCancelled(_ context.Context, txnID id.VerificationTransactionID, code event.VerificationCancelCode, reason string) {
	v.send(&matrixclient.Verification{
		TransactionID: txnID,
		Stage:         matrixclient.VerificationCancelled,
		CancelCode:    code,
		Reason:        reason,
	})
}

func (v verificationCallbacks) VerificationDone(_ context.Context, txnID id.VerificationTransactionID, method event.VerificationMethod) {
	v.send(&matrixclient.Verification{
		TransactionID: txnID,
		Stage:         matrixclient.VerificationDone,
		Method:        method,
	})
}
