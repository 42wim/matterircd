# matterircd
[![Join the IRC chat at https://webchat.freenode.net/?channels=matterircd](https://img.shields.io/badge/IRC-matterircd-green.svg)](https://webchat.freenode.net/?channels=matterircd)

Minimal IRC server which integrates with [mattermost](https://www.mattermost.org) and [slack](https://www.slack.com)
Tested on Windows / Linux

Most of the work happens in [mm-go-irckit](https://github.com/42wim/mm-go-irckit) (based on github.com/shazow/go-irckit)

# Docker
Run the irc server on port 6667. You'll need to specify -bind 0.0.0.0:6667 otherwise it only listens on 127.0.0.1 in the container.

```
docker run -p 6667:6667 42wim/matterircd:latest -bind 0.0.0.0:6667
```

Now you can connect with your IRC client to port 6667 on your docker host.

# Compatibility
* Matterircd v0.13.0 works with mattermost 3.5.x - 3.10.0, 4.0.x [4.0.0 release](https://github.com/mattermost/platform/releases/tag/v4.0.0)
* Matterircd v0.14.0-dev also supports slack

Master branch of matterircd should always work against latest STABLE mattermost release.  
If you want to run matterircd with mattermost DEV builds, use the develop branch of matterircd.

# Features

* support direct messages / private channels / edited messages
* auto-join/leave to same channels as on mattermost
* reconnects with backoff on mattermost restarts
* support multiple users
* support channel backlog (messages when you're disconnected from IRC/mattermost)
* search messages (/msg mattermost search query)
* scrollback support (/msg mattermost scrollback #channel limit)
* restrict to specified mattermost instances
* set default team/server
* WHOIS, WHO, JOIN, LEAVE, NICK, LIST, ISON, PRIVMSG, MODE, TOPIC, LUSERS, AWAY, KICK, INVITE support
* support TLS (ssl)
* support LDAP logins (mattermost enterprise) (use your ldap account/pass to login)
* &users channel that contains members of all teams (if mattermost is so configured) for easy messaging
* supports mattermost roles (shows admins with @ status for now)
* gitlab auth hack by using mmtoken cookie (see https://github.com/42wim/matterircd/issues/29)

# Binaries

You can find the binaries [here](https://github.com/42wim/matterircd/releases/)
* For use with mattermost 3.5.0 and higher [v0.13.0](https://github.com/42wim/matterircd/releases/tag/v0.13.0)

# Building

Go 1.6+ is required 
Make sure you have [Go](https://golang.org/doc/install) properly installed, including setting up your [GOPATH] (https://golang.org/doc/code.html#GOPATH)

```
cd $GOPATH
go get github.com/42wim/matterircd
```

You should now have matterircd binary in the bin directory:

```
$ ls bin/
matterircd
```

# Usage

```
Usage of ./matterircd:
  -bind string
        interface:port to bind to. (default "127.0.0.1:6667")
  -debug
        enable debug logging
  -interface string
        interface to bind to (deprecated: use -bind)
  -mminsecure
        use http connection to mattermost
  -mmserver string
        specify default mattermost server/instance
  -mmteam string
        specify default mattermost team
  -port int
        Port to bind to (deprecated: use -bind)
  -restrict string
        only allow connection to specified mattermost server/instances. Space delimited
  -tlsbind string
        interface:port to bind to. (e.g 127.0.0.1:6697)
  -tlsdir string
        directory to look for key.pem and cert.pem. (default ".")
  -version
        show version
```

Matterircd will listen by default on localhost port 6667.
Connect with your favorite irc-client to localhost:6667

For TLS support you'll need to generate certificates.   
You can use this program [generate_cert.go](https://golang.org/src/crypto/tls/generate_cert.go) to generate key.pem and cert.pem

## Mattermost user commands

Login

```
/msg mattermost login <server> <team> <username/email> <password>
```

Or if it is set up to only allow one host:

```
/msg mattermost login <username/email> <password>
```

Search
```
/msg mattermost search query
```

Scrollback
```
/msg mattermost scrollback <channel> <limit>
e.g. /msg mattermost scrollback #bugs 100 shows the last 100 messages of #bugs
```
## Slack user commands
Get a slack token on https://api.slack.com/custom-integrations/legacy-tokens

Login

```
/msg slack login <token>
```

## Docker

A docker image for easily setting up and running matterircd on a server is available at [docker hub](https://hub.docker.com/r/42wim/matterircd/).

## Examples

1. Login to your favorite mattermost service by sending a message to the mattermost user
![login](http://snag.gy/aAop5.jpg)

2. You'll be auto-joined to all the channels you're a member of
![channel](http://snag.gy/IzlXR.jpg)

3. Chat away
![chat](http://snag.gy/JyFd7.jpg)
![mmchat](http://snag.gy/3qMd1.jpg)

Also works with windows ;-)
![windows](http://snag.gy/cGSCA.jpg)

If you use chrome, you can easily test with [circ](https://chrome.google.com/webstore/detail/circ/bebigdkelppomhhjaaianniiifjbgocn?hl=en-US)
