# matterircd

Minimal IRC server which integrates with [mattermost](https://www.mattermost.org)  
Tested on Windows / Linux

Most of the work happens in [mm-go-irckit](https://github.com/42wim/mm-go-irckit) (based on github.com/shazow/go-irckit)

# Compatibility

* Matterircd v0.3 is needed for mattermost 1.4.0 and higher
* Matterircd v0.2 works only on mattermost < 1.4.0

Master branch of matterircd should always work against latest STABLE mattermost release.  
(note: if you use mattermost docker image, you can run the image with 2.0 or 2.1 tag)   
If you want to run matterircd with mattermost DEV builds, use the develop branch of matterircd.

# Features

* support direct messages / private channels
* auto-join/leave to same channels as on mattermost
* reconnects with backoff on mattermost restarts
* support multiple users
* support channel backlog (messages when you're disconnected from IRC/mattermost)
* search messages (/msg mattermost search query)
* scrollback support (/msg mattermost scrollback #channel limit)
* restrict to specified mattermost instances
* set default team/server
* WHOIS, WHO, JOIN, LEAVE, NICK, LIST, ISON, PRIVMSG, MODE, TOPIC support

# Binaries

You can find the binaries [here](https://github.com/42wim/matterircd/releases/)
* For use with mattermost 1.4.0+ [v0.3](https://github.com/42wim/matterircd/releases/tag/v0.3)
* For use with mattermost <1.4.0 [v0.2](https://github.com/42wim/matterircd/releases/tag/v0.2)

# Usage

```
Usage of matterircd:
  -debug=false: enable debug logging
  -interface="127.0.0.1": interface to bind to
  -mminsecure=false: use http connection to mattermost
  -mmserver="": specify default mattermost server/instance
  -mmteam="": specify default mattermost team
  -port=6667: Port to bind to
  -restrict="": only allow connection to specified mattermost server/instances. Space delimited
```

Matterircd will listen by default on localhost port 6667.
Connect with your favorite irc-client to localhost:6667

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

## Docker

A docker image for easily setting up and running matterircd on a server is available at [docker hub](https://hub.docker.com/r/xyproto/matterircd/).

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

## Caveats

Matterircd sends a "Unicode Character 'MONGOLIAN VOWEL SEPARATOR' (U+180E)" at the end of every line to mattermost, more information about this can be found in ![this issue](https://github.com/42wim/matterircd/issues/24)

