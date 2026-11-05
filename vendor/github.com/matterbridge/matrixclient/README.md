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
the device keys. Devices are not verified or cross-signed, so other clients
show the session as unverified.

## Mattermost concepts

- Teams are Matrix spaces (`Teams`, `TeamChannels`, `RoomTeams`, `CreateTeam`, `AddToTeam`).
- Matrix has no bot flag, `IsBot` is true for users that only ever sent `m.notice` messages.
- `PublicLink` gives an unauthenticated media URL, `DownloadURL` the authenticated one.
- `Ping` is skipped while syncs are arriving and `Reconnect` restarts the sync with a fresh login.
