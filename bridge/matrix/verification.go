package matrix

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/utils"
	"github.com/matterbridge/matrixclient"
)

// verificationQueueSize bounds the updates waiting for verificationLoop.
const verificationQueueSize = 16

// sasEmojiAliases maps the SAS descriptions to emoji shortcodes. The SAS runes
// of some emoji lack the variation selector, so terminals draw them as text.
var sasEmojiAliases = map[string]string{
	"Panda": "panda_face", "Flower": "tulip", "Tree": "deciduous_tree", "Globe": "earth_asia",
	"Moon": "crescent_moon", "Smiley": "grinning", "Hat": "tophat", "Glasses": "eyeglasses",
	"Spanner": "wrench", "Thumbs Up": "+1", "Clock": "alarm_clock", "Light Bulb": "bulb",
	"Book": "closed_book", "Pencil": "pencil2", "Flag": "checkered_flag", "Train": "steam_locomotive",
	"Bicycle": "bike", "Ball": "soccer", "Folder": "file_folder", "Pin": "pushpin",
	"Aeroplane": "airplane", //nolint:misspell // the description string used by the SAS spec
	"Horse": "racehorse", "Umbrella": "umbrella2",
}

var errNoVerification = errors.New("no verification in progress, use VERIFY START")

// OnVerification implements matrixclient.VerificationHandler. It must not block
// or call verification methods, as the engine may hold its transaction lock.
func (m *Matrix) OnVerification(v *matrixclient.Verification) {
	select {
	case m.verifyQueue <- v:
	default:
		logger.Warnf("verification queue full, dropping %s update of %s", v.Stage, v.TransactionID)
	}
}

func (m *Matrix) verificationLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case v := <-m.verifyQueue:
			m.handleVerification(v)
		}
	}
}

// setVerifyTxn makes txn the active verification. Any displayed SAS belongs to
// the previous transaction, so it no longer allows a confirmation.
func (m *Matrix) setVerifyTxn(txn id.VerificationTransactionID) {
	m.verifyMu.Lock()
	m.verifyTxn = txn
	m.verifySAS = false
	m.verifyMu.Unlock()
}

// currentVerifyTxn returns the active transaction, and whether its SAS was shown.
func (m *Matrix) currentVerifyTxn() (id.VerificationTransactionID, bool) {
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()

	return m.verifyTxn, m.verifySAS
}

// clearVerifyTxn forgets txn, unless a newer transaction replaced it.
func (m *Matrix) clearVerifyTxn(txn id.VerificationTransactionID) {
	m.verifyMu.Lock()
	if m.verifyTxn == txn {
		m.verifyTxn = ""
		m.verifySAS = false
	}
	m.verifyMu.Unlock()
}

// updateVerifyTxn applies an update to the active transaction. It reports false
// for a superseded transaction, whose updates must not reach the user. A new
// request replaces the active one, and a ready update only adopts a
// transaction when none is active, as it follows our own start.
func (m *Matrix) updateVerifyTxn(v *matrixclient.Verification) bool {
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()

	switch {
	case v.Stage == matrixclient.VerificationRequested:
		m.verifyTxn = v.TransactionID
		m.verifySAS = false
	case v.Stage == matrixclient.VerificationReady && m.verifyTxn == "":
		m.verifyTxn = v.TransactionID
	case m.verifyTxn != v.TransactionID:
		return false
	case v.Stage == matrixclient.VerificationSAS:
		m.verifySAS = true
	case v.Stage == matrixclient.VerificationCancelled, v.Stage == matrixclient.VerificationDone:
		m.verifyTxn = ""
		m.verifySAS = false
	}

	return true
}

// sanitizeNotice replaces the characters that would end the IRC line, as the
// reason and codes of a cancellation are chosen by the remote device.
func sanitizeNotice(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}

		return r
	}, s)
}

func (m *Matrix) handleVerification(v *matrixclient.Verification) {
	if !m.updateVerifyTxn(v) {
		return
	}

	var text string

	switch v.Stage {
	case matrixclient.VerificationRequested:
		text = fmt.Sprintf("verification requested by %s (device %s), use VERIFY ACCEPT to accept or VERIFY CANCEL to decline", v.UserID, v.DeviceID)
	case matrixclient.VerificationReady:
		if !v.SupportsSAS {
			text = "the other device does not support emoji verification, use VERIFY CANCEL"

			break
		}

		text = "verification ready, use VERIFY SAS to begin the comparison (or begin it on the other device)"
	case matrixclient.VerificationSAS:
		text = "compare with the other device: " + formatSAS(v) + " - if identical use VERIFY CONFIRM, else VERIFY MISMATCH"
	case matrixclient.VerificationCancelled:
		text = fmt.Sprintf("verification canceled: %s (%s)", v.Reason, v.CancelCode)
	case matrixclient.VerificationDone:
		text = "verification complete, this session is now verified"
	default:
		return
	}

	m.sendEventCtx(m.ctx, &bridge.Event{Type: "banner_change", Data: &bridge.BannerChangeEvent{Text: sanitizeNotice(text)}})
}

func formatSAS(v *matrixclient.Verification) string {
	var sb strings.Builder

	for i, e := range v.Emojis {
		if i > 0 {
			sb.WriteString("  ")
		}

		if i >= len(v.EmojiDescriptions) {
			sb.WriteRune(e)

			continue
		}

		sb.WriteString(sasEmoji(e, v.EmojiDescriptions[i]) + " " + v.EmojiDescriptions[i])
	}

	if len(v.Decimals) > 0 {
		if sb.Len() > 0 {
			sb.WriteString(" | ")
		}

		for i, d := range v.Decimals {
			if i > 0 {
				sb.WriteByte(' ')
			}

			fmt.Fprint(&sb, d)
		}
	}

	return sb.String()
}

// Verify runs one verification step of this session: start, accept, sas,
// confirm, mismatch or cancel. Progress and completion are reported as
// notices from OnVerification, not by the return value.
func (m *Matrix) Verify(ctx context.Context, action string) (string, error) {
	if action == "start" {
		txn, err := m.mc.StartVerification(ctx, m.mc.UserID())
		if err != nil {
			return "", err
		}

		m.setVerifyTxn(txn)

		return "verification requested, accept it on your other device", nil
	}

	txn, shown := m.currentVerifyTxn()
	if txn == "" {
		return "", errNoVerification
	}

	switch action {
	case "accept":
		return "accepted", m.mc.AcceptVerification(ctx, txn)
	case "sas":
		return "starting emoji comparison", m.mc.StartSAS(ctx, txn)
	case "confirm":
		if !shown {
			return "", errors.New("no emoji comparison shown for the active verification")
		}

		return "confirmed, waiting for the other device", m.mc.ConfirmSAS(ctx, txn)
	case "mismatch":
		return "canceled, the symbols did not match", m.cancelVerification(ctx, txn, event.VerificationCancelCodeSASMismatch, "SAS mismatch")
	case "cancel":
		return "canceled", m.cancelVerification(ctx, txn, event.VerificationCancelCodeUser, "canceled by user")
	}

	return "", errors.New("unknown action " + action)
}

func (m *Matrix) cancelVerification(ctx context.Context, txn id.VerificationTransactionID, code event.VerificationCancelCode, reason string) error {
	err := m.mc.CancelVerification(ctx, txn, code, reason)
	if err != nil {
		return err
	}

	m.clearVerifyTxn(txn)

	return nil
}

// sasEmoji returns the fully qualified emoji for a SAS symbol. An alias that
// resolves to a different symbol is ignored, as the user compares symbols.
func sasEmoji(r rune, description string) string {
	alias, ok := sasEmojiAliases[description]
	if !ok {
		alias = strings.ReplaceAll(strings.ToLower(description), " ", "_")
	}

	if e, found := utils.EmojiFromAlias(alias, nil); found && strings.HasPrefix(e, string(r)) {
		return e
	}

	if r < 0x1F000 {
		return string(r) + "\ufe0f"
	}

	return string(r)
}
