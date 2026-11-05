package e2ee

import (
	"context"
	"slices"

	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matterbridge/matrixclient"
)

// InitVerification implements matrixclient.VerificationProvider.
func (h helper) InitVerification(ctx context.Context, notify func(*matrixclient.Verification)) (matrixclient.VerificationController, error) {
	callbacks := verificationCallbacks{ctx: ctx, notify: notify}
	vh := verificationhelper.NewVerificationHelper(h.client, h.Machine(), nil, callbacks, false, false, true)

	err := vh.Init(ctx)
	if err != nil {
		return nil, err
	}

	h.client.Verification = vh

	return vh, nil
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
