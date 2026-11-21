# rocketchatclient

Small Rocket.Chat client library for [matterircd](https://github.com/42wim/matterircd) and [matterbridge](https://github.com/42wim/matterbridge), modeled on [matterclient](https://github.com/matterbridge/matterclient) and [matrixclient](https://github.com/matterbridge/matrixclient). Isolates Rocket.Chat's REST API and Meteor DDP WebSocket protocol behind normalized Go types and `Handler` callbacks.

## Usage

```go
c, err := rocketchatclient.New(ctx, rocketchatclient.Credentials{
	Server: "https://chat.example.com",
	Login:  "alice",
	Pass:   "secret",
}, handler)
if err != nil {
	return err
}
defer c.Logout()
```

### Options

- `WithSkipTLSVerify(bool)` — disables TLS certificate verification for REST and WebSocket connections.
- `WithNoTLS(bool)` — defaults bare hostnames to `http://` and `ws://` instead of `https://` and `wss://`.
- `WithTimeout(time.Duration)` — sets the dial and TLS handshake timeout (10s default).
- `WithUserAgent(string)` — sets the `User-Agent` header for REST and WebSocket requests.
- `WithLRUSizes(sentIDs, messages, reactions int)` — overrides the bounded LRU cache capacities.

### Logging

- `SetLogLevel(level)` configures the `rocketchatclient` logger (`info`, `debug`, `trace`, etc.).
- `SetLogAPICalls(level)` configures the REST API request logger (`"warn"` logs requests and status codes without query strings).

## Authentication

Two authentication modes are supported via `Credentials`:

- **Username / password (+ optional 2FA):** Set `Login`, `Pass`, and optionally `TwoFactorCode`. `New` calls `POST /api/v1/login` and receives `userId` and `authToken`. `Logout` calls `POST /api/v1/logout` to revoke the session.
- **Personal Access Token (PAT) or browser session token:** Set `Token` to `<userId>:<authToken>` (or `token=<userId>:<authToken>`), which also covers SSO/OAuth/SAML users extracting `rc_uid` and `rc_token` cookies. `New` validates the token against `GET /api/v1/me`. `Logout` never revokes user-supplied tokens.

## Lifecycle & Handler callbacks

`New` authenticates synchronously over REST so invalid credentials fail immediately, then starts the background DDP WebSocket and event consumer goroutines.

1. Connects to `wss://<host>/websocket` (or `ws://`) with dedicated single-reader and single-writer goroutines.
2. Completes the DDP `connect` handshake and authenticates via `method: "login"` (`{"resume": authToken}`).
3. Hydrates initial state from `GET /api/v1/subscriptions.get`, `GET /api/v1/rooms.get`, and paginated `GET /api/v1/users.list` (tolerating `401`/`403` when `users.list` is restricted by server permissions).
4. Subscribes to core DDP streams:
   - `stream-room-messages` (`__my_messages__`)
   - `stream-notify-user` (`<userId>/rooms-changed`, `<userId>/subscriptions-changed`)
   - `stream-notify-logged` (`user-status`, `Users:NameChanged`)
5. Invokes `Handler.Ready()` once hydration completes and streams are active.

All `Handler` callbacks (`Ready`, `OnMessage`, `OnEdit`, `OnDelete`, `OnReaction`, `OnRoomChange`, `OnPresence`) and optional callbacks (`InviteHandler.OnInvite`, `MemberHandler.OnMember`, `ReconnectHandler.OnReconnect`) execute sequentially on a single event consumer goroutine and must never block.

Heartbeats answer server `ping` frames immediately (preserving frame `id`), send periodic client `ping` frames, and enforce read deadlines so dead connections reconnect automatically with exponential backoff and jitter.

## State cache & helpers

`sync.RWMutex`-protected cache accessors return snapshots safe for concurrent use:

- `Me()`, `UserID()`
- `User(id)`, `UserByUsername(username)`, `Users()`, `GetUserInfo(ctx, userID, username)`
- `Room(id)`, `Rooms()`, `Subscription(roomID)`, `Subscriptions()`, `Invitations()`
- `DownloadURL(fileID, fileName)` returns `<serverURL>/file-upload/<fileID>/<fileName>`

## Rocket.Chat / Mattermost concepts

- **Room types:** `"c"` (`RoomTypeChannel`) for public channels, `"p"` (`RoomTypePrivate`) for private groups, and `"d"` (`RoomTypeDirect`) for 1:1 and multi-user direct messages (`UIDs` determines peer count).
- **Timestamps:** `Timestamp` transparently decodes both REST ISO-8601 strings and Meteor DDP EJSON `{"$date": ms}` objects.
- **Rate limits:** REST calls honor `429 Too Many Requests` backoff using `X-Ratelimit-Reset` and `Retry-After`.
