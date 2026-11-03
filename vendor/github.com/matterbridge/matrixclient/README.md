# matrixclient

Small Matrix client library for matterircd/matterbridge, modelled on
[matterclient](https://github.com/matterbridge/matterclient). Wraps mautrix:
password login, sync loop, room/DM/user tracking, send (incl. threads),
and message/reaction callbacks through the `Handler` interface.

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
