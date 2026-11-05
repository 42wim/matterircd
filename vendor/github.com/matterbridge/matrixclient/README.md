# matrixclient

Small Matrix client library for matterircd/matterbridge, modelled on
[matterclient](https://github.com/matterbridge/matterclient). Wraps mautrix:
password login, sync loop, room/DM/user tracking, send (incl. threads),
and message/reaction callbacks through the `Handler` interface.

## Room invitations

`Invitations()` returns pending invitations for the logged-in user, including
direct rooms. Each includes the room ID, inviter (`Sender`), optional room name,
alias and reason, and whether it is direct. Invited rooms are not listed by
`Rooms()` or selected by `DirectRoom()`, and are never automatically joined.

Implement the optional `InviteHandler.OnInvite(*Invitation)` to receive new or
changed invitations after the initial sync. Invitations received while offline
are available through `Invitations()` when `Handler.Ready` runs. Read that
snapshot once your event consumer is running; initial invitations are not
replayed through `OnInvite`. Deduplicate by room ID if consumer startup overlaps
live callbacks. Callbacks run on the sync goroutine and must not block.
For compatibility, live self-invites also reach `MemberHandler.OnMember` with
`MembershipInvite`; use one callback path to avoid duplicate notifications.

Accept with `Join(ctx, string(invite.RoomID))`; Matrix has no separate accept API.
Reject with `Leave(ctx, invite.RoomID)`. Surface request errors to the user;
failed requests keep the invitation pending. Successful joins, rejections and
revocations remove it from `Invitations()` when synced. An IRC bridge can render
`OnInvite` as an IRC `INVITE` and use `/join` to call `Join` with the room ID.
Accepted direct invitations retain their DM classification across `m.direct`
updates. Rooms observed leaving are excluded from `DirectRoom()` and `Rooms()`
until they are joined again, even if stale `m.direct` data still lists them.
Their DM classification is retained for final or late-decrypted incoming events
and reused when rejoining without a new invitation or `m.direct` update.
`Join` persists an accepted direct invitation to `m.direct` so it survives a
restart. Restoring DM associations does not add members; room membership follows
membership events independently of direct-room classification.

## Encrypted rooms

End-to-end encryption is opt-in through the `e2ee` subpackage, as it needs cgo
(sqlite for the key store) and either libolm or the pure Go `goolm` build tag:

```go
c, err := matrixclient.New(ctx, cred, handler, e2ee.Factory(e2ee.Config{
	DBPath:    "keys.db",
	PickleKey: key,
}))
```

Build with `go build -tags goolm`. Keep the database and pickle key, they hold
the device keys. Interactive device verification is available through the
optional `VerificationHandler` below; without it the session stays unverified.

## Interactive device verification

With `e2ee.Factory`, implement `VerificationHandler.OnVerification(*Verification)`
on the handler passed to `New`. This enables mautrix's SAS engine before syncing,
including incoming to-device requests received during the initial sync. No
cryptography or raw to-device event handling is needed in matterircd.

Queue these callbacks to a separate consumer. They may run concurrently, before
`Handler.Ready`, or while a verification API call holds a transaction lock.
**Do not call verification methods directly inside the callback**, and do not
block it waiting for user input. Make the consumer ready before calling `New`;
it may need to wait for `New` to return before using the client.

Correlate updates and user commands by `TransactionID`:

| Stage | What matterircd should display/do |
| --- | --- |
| `VerificationRequested` | Show the requesting `UserID` and `DeviceID`. Ask the user to accept; call `AcceptVerification(ctx, txnID)` on acceptance. |
| `VerificationReady` | Acceptance is complete, not verification. If `SupportsSAS`, call `StartSAS(ctx, txnID)` to begin comparing; the peer may also initiate this exchange. |
| `VerificationSAS` | Display `Emojis` with `EmojiDescriptions`, or the three `Decimals`, in order. Ask whether they match the other device. Call `ConfirmSAS(ctx, txnID)` only after explicit confirmation. |
| `VerificationCancelled` | Show `CancelCode` and `Reason`, including timeout or cryptographic mismatch, and discard the pending UI. |
| `VerificationDone` | Report successful verification and discard the pending UI. |

For the other client's "Start verification on the other device" screen, either
initiate from matterircd with `StartVerification(ctx, c.UserID())`, or initiate
from the other client and accept its `VerificationRequested` update in matterircd.
Outgoing requests go to the user's other devices, not a single device ID; the
first device accepting is identified in `VerificationReady.DeviceID`. Keep the
request's user/device information for later updates, which carry the transaction
ID but do not repeat all peer information.

Reject or cancel with `CancelVerification(ctx, txnID,
event.VerificationCancelCodeUser, reason)`; use
`event.VerificationCancelCodeSASMismatch` if the SAS differs. Surface every API
error to the user. Local cancellation succeeds via its return value (there is no
additional local callback). Never report success on acceptance or confirmation
alone: wait for `VerificationDone`, after MAC validation and completion.

Verification methods return `ErrVerificationUnavailable` without a verification
handler and a supporting crypto provider. Pending transactions are in memory
and cannot be resumed after a restart. QR verification, recovery-key entry and
cross-signing private-key recovery are not exposed by this API.

## Mattermost concepts

- Teams are Matrix spaces (`Teams`, `TeamChannels`, `RoomTeams`, `CreateTeam`, `AddToTeam`).
- Matrix has no bot flag, `IsBot` is true for users that only ever sent `m.notice` messages.
- `PublicLink` gives an unauthenticated media URL, `DownloadURL` the authenticated one.
- `Ping` is skipped while syncs are arriving and `Reconnect` restarts the sync with a fresh login.
