# matterircd
<!-- TOC -->

- [matterircd](#matterircd)
    - [Compatibility](#compatibility)
    - [Features](#features)
    - [Binaries](#binaries)
    - [Building](#building)
    - [Config file](#config-file)
    - [Usage](#usage)
        - [Mattermost user commands](#mattermost-user-commands)
        - [Slack user commands](#slack-user-commands)
        - [Mastodon user commands](#mastodon-user-commands)
    - [Docker](#docker)
    - [FreeBSD](#freebsd)
    - [Support/questions](#supportquestions)
    - [FAQ](#faq)
        - [mattermost login with sso/gitlab](#mattermost-login-with-ssogitlab)
        - [slack sso login / xoxc tokens](#slack-sso-login--xoxc-tokens)
    - [Guides](#guides)
    - [Related](#related)

<!-- /TOC -->

[![Join the IRC chat at https://web.libera.chat/gamja/?channels=#matterircd](https://img.shields.io/badge/IRC-matterircd-green.svg)](https://web.libera.chat/gamja/?channels=#matterircd)

Minimal IRC server which integrates with [mattermost](https://www.mattermost.org) and [slack](https://www.slack.com).  
Tested on FreeBSD / Linux / Windows

## Compatibility

- Matterircd works with slack, mastodon and mattermost 10.x, 11.x

Master branch of matterircd should always work against latest STABLE mattermost release.

## Features

- support direct messages / private channels / edited messages / deleted messages / reactions
- auto-join/leave to same channels as on mattermost
- reconnects with backoff on mattermost restarts
- support multiple users
- support channel/direct message backlog (messages when you're disconnected from IRC/mattermost)
- search messages (/msg mattermost search query)
- scrollback support (/msg mattermost scrollback #channel limit)
- away support
- restrict to specified mattermost instances
- set default team/server
- WHOIS, WHO, JOIN, LEAVE, NICK, LIST, ISON, PRIVMSG, MODE, TOPIC, LUSERS, AWAY, KICK, INVITE support
- support TLS (ssl)
- support unix sockets
- support LDAP logins (mattermost enterprise) (use your ldap account/pass to login)
- &users channel that contains members of all teams (if mattermost is so configured) for easy messaging
- support for including/excluding channels from showing up in irc
- supports mattermost roles (shows admins with @ status for now)
- gitlab auth hack by using mmtoken cookie (see <https://github.com/42wim/matterircd/issues/29>)
- mattermost personal token support
- support multiline pasting
- prefixcontext option for mattermost (see <https://github.com/42wim/matterircd/blob/master/prefixcontext.md>)
  - threading support
  - reactions support
- ....

## Binaries

You can find the binaries [here](https://github.com/42wim/matterircd/releases/latest)

## Building

Go 1.26+ is required

```bash
go build -tags goolm -ldflags="-X main.githash=$(git describe --always --dirty --exclude '*')"
```

The `goolm` tag enables Matrix end-to-end encryption, which also needs cgo (`CGO_ENABLED=1` and a C compiler).

With encryption enabled, matterircd reports on login whether the session (device) is verified, and if not, how to verify it. Verify the session from the service bot with `VERIFY START`, accept it on your other device, then `VERIFY SAS`, compare the emoji and `VERIFY CONFIRM` (or `VERIFY MISMATCH`). Incoming requests are answered with `VERIFY ACCEPT` or `VERIFY CANCEL`. The request goes to all your other devices, not a selected one. To verify another user, use `VERIFY START <user>` (a direct message room with them is required), then continue with the same steps.

Or:

```bash
go install -tags goolm github.com/42wim/matterircd
```

You should now have matterircd binary in the bin directory:

```bash
$ ls ~/go/bin/
matterircd
```

## Config file

See [matterircd.toml.example](https://github.com/42wim/matterircd/blob/master/matterircd.toml.example).  
Run with `matterircd --conf matterircd.toml`

## Usage

```bash
Usage of ./matterircd:
      --bind string      interface:port to bind to, or a path to bind to a Unix socket. (default "127.0.0.1:6667")
      --conf string      config file (default "matterircd.toml")
      --debug            enable debug logging
      --tlsbind string   interface:port to bind to. (e.g 127.0.0.1:6697)
      --tlsdir string    directory to look for key.pem and cert.pem. (default ".")
      --version          show version
```

Matterircd will listen by default on localhost port 6667.
Connect with your favorite irc-client to localhost:6667

For TLS support you'll need to generate certificates.  
You can use this program [generate_cert.go](https://golang.org/src/crypto/tls/generate_cert.go) to generate key.pem and cert.pem

### Mattermost user commands

Login with user/pass

```
/msg mattermost login <server> <team> <username/email> <password>
```

Login with personal token

```
/msg mattermost login <server> <team> <username/email> token=<yourpersonaltoken>
```

Login with MFA token

```
/msg mattermost login <server> <team> <username/email> <password> MFAToken=<mfatoken>
```

Search
```
/msg mattermost search query
/msg mattermost search 10 query
```
Search modifiers are also supported, see https://docs.mattermost.com/collaborate/search-for-messages.html#search-modifiers.
```
/msg mattermost search 10 some-string from: hloeung after: 2023-09-27 in: matterircd
```

Scrollback
```
/msg mattermost scrollback #<channel>|<user>|<post/thread ID> <limit>
```
e.g. `/msg mattermost scrollback #bugs 100` shows the last 100 messages of *#bugs*
e.g. `/msg mattermost scrollback zdofdf1nctgsj87xgt6oco1a3w 0` shows all messages from the thread with root/parent *zdofdf1nctgsj87xgt6oco1a3w*

Mark messages in a channel/from a user as read (when DisableAutoView is set).
```
/msg mattermost updatelastviewed <channel>
/msg mattermost updatelastviewed <username>
```

Part/leave
```
/msg mattermost part #mychannel
```

### Slack user commands

If you still have a legacy Slack token (`xoxp-...`, previously from <https://api.slack.com/custom-integrations/legacy-tokens>), you can log in directly with it:

```
/msg slack login <token>
```

Otherwise, use browser session credentials (`xoxc-` token + `d` cookie — see [slack sso login / xoxc tokens](#slack-sso-login--xoxc-tokens) below, noting the required trailing `;`):

```
/msg slack login xoxc-XXXX|d=xoxd-XXXX;
```

Or use team/login/pass to login
```
/msg slack login <team> <login> <password>
```
After login it'll show you a token you can use for the token login

## Docker

A docker image for easily setting up and running matterircd on a server is available at [docker hub](https://hub.docker.com/r/42wim/matterircd/).

Run the irc server on port 6667. You'll need to specify -bind 0.0.0.0:6667 otherwise it only listens on 127.0.0.1 in the container.

```
docker run -p 6667:6667 42wim/matterircd:latest --bind 0.0.0.0:6667
```

Now you can connect with your IRC client to port 6667 on your docker host.

### Matrix user commands

(EXPERIMENTAL, work in progress)

Login

```
/msg matrix login <server> <login> <pass>
```

or with an existing access token (it cannot be renewed once it expires)

```
/msg matrix login <server> <token>
```

Room invitations are shown as an IRC `INVITE` with a message from the `matrix` user. Accept one with the `/join` it names, e.g. `/join #!roomid:example.org`.

Settings are in the `[matrix]` section in [matterircd.toml.example](https://github.com/42wim/matterircd/blob/master/matterircd.toml.example)

### Mastodon user commands

(still WIP)

Configure your mastodon settings  
See the `[mastodon]` section in [matterircd.toml.example](https://github.com/42wim/matterircd/blob/master/matterircd.toml.example)

Login

```
/msg mastodon login
```

You'll see your timeline in #mastodon (automatically joined to that channel)

## FreeBSD

Install the port.
```
# pkg install matterircd
```
Or with a local ports tree.
```
$ cd /usr/ports/net-im/matterircd
# make install clean
```

Enable the service.
```
echo "matterircd_enable="YES" >> /etc/rc.conf
```
Copy the default configuration and modify to your needs.
```
# cp /usr/local/etc/matterircd/matterircd.toml.sample /usr/local/etc/matterircd/matterircd.toml
```
Start the service.
```
# service matterircd start
```

## Support/questions

We're also on the `#matterircd` channel on irc.libera.chat

## FAQ

### can I use matterircd for multiple mattermost or slack servers?

Yes, but not in the same connection (would cause problems with channel/user name conflicts).  
matterircd supports multiple users, so you can configure your IRC client to have multiple networks that all connect to the same matterircd server.

### mattermost login with sso/gitlab

You'll need to get the `MMAUTHTOKEN` from your cookies, login via the browser first.  
Then in chrome run F12 - application - Storage/cookies - select your mattermostdomain and fetch the `MMAUTHTOKEN`

Now login with `/msg mattermost login <username> MMAUTHTOKEN=<mytoken>`  
(if this doesn't work, try `/msg mattermost login <username> token=<mytoken>`)

See <https://github.com/42wim/matterircd/issues/29> for more information

Also see [#98](https://github.com/42wim/matterircd/issues/98#issuecomment-307308876) for a script that fetches it for you.

### slack sso login / xoxc tokens

Taken from: <https://github.com/insomniacslk/irc-slack>

Since legacy `xoxp-` tokens are deprecated, you can use your browser session's `xoxc-` token and `d` (`xoxd-...`) cookie:

1. Log into your Slack workspace in a browser (`https://app.slack.com/client/...`) and open Developer Tools (`F12`).
2. **Get the `xoxc-` token** using either method:
   - **Console**: Run the following in the DevTools **Console** tab and copy your workspace's `token` (`xoxc-...`):
     ```js
     Object.values(JSON.parse(localStorage.localConfig_v2).teams).map(t => ({name: t.name, token: t.token}))
     ```
   - **Network tab**: Open the **Network** tab, filter for `api/`, trigger an action or reload, click a request to `slack.com/api/...`, and copy the `token` (`xoxc-...`) from the request payload/form data.
3. **Get the `d` cookie (`xoxd-...`)**:
   - Open DevTools → **Application** (Chrome/Edge) or **Storage** (Firefox) → **Cookies** → `https://app.slack.com` (or `.slack.com`).
   - Copy the value of the cookie named `d` (it starts with `xoxd-`). Keep any URL encoding (such as `%2B`, `%2F`, `%3D`) as-is—do not decode it.
4. **Log in via IRC**:
   Concatenate the `xoxc-` token and `d=` cookie with `|`. **You MUST include the trailing semicolon `;` at the end**, otherwise login will fail with `auth cookie must have the format 'd=XXX;'`:

   ```
   /msg slack login xoxc-XXXX|d=xoxd-XXXX;
   ```

   Keep that browser session signed in (closing the tab is fine, but clicking "Sign out" in the browser will invalidate the token and cookie).

## Guides

Here are some external guides and documentation that might help you get up and
running more quickly:

- [Breaking out of the Slack walled garden](https://purpleidea.com/blog/2018/06/22/breaking-out-of-the-slack-walled-garden/)

## Contributors

[![Contributors](https://contrib.rocks/image?repo=42wim/matterircd)](https://github.com/42wim/matterircd/graphs/contributors)

## Related

- [matterclient](https://github.com/matterbridge/matterclient) - Minimal library used to interact with the Mattermost chat server.
- [matterircd-complete](https://github.com/hloeung/matterircd-complete) - better irssi/mattermost/matterircd integration.
