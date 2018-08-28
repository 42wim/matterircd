# v0.18.3

## Bugfix
* slack: api changed, show uploaded files again

Because of changes in slack API and the forced use of pagination, big channels with lots of users can take a while to load.

# v0.18.2
## Bugfix
* slack: fix panic on websocket bug #189, #196

# v0.18.1
## New features
* mattermost: support mattermost 5.x

# v0.18.0
## New features
* general: Add debugmode true/false message in banner
* irc: Add PrefixMainTeam setting. Also set the main team name as prefix in the irc-channel. See matterircd.toml.example
* slack: Add support for login <team> <user> <pass> for slack (as addition to login <token>)

## Bugfix
* mattermost: update channels when adding/removing users to new channel. Alsso join channel when we are added. Closes 42wim/matterircd#178
* irc: fix NAMES reply to send entire member list 
* irc: Use service for on-join topic changes (instead of your own username)
* irc: Handle \r, ACTION and colour sanitization everywhere
* irc: Fix concurrent map read/write. Closes 42wim/matterircd#188
* slack: Make sure to return for not implemented functions in slack. Closes 42wim/matterircd#186
* slack: Replace spaces to underscore in botnames. Closes 42wim/matterircd#184

# v0.17.3
## Bugfix
* slack: Fix issues with bots with spaces in the name
* mattermost: Actually join/remove users to channel when they join, not when they say something #113
* mattermost: Join/remove users when they're added by someone else. Use a system message to show this #175

# v0.17.2
## Bugfix
* mattermost: Fix message looping issue

# v0.17.1
## New features
* general: enable login via irc PASS command during handshake instead of PRIVMSG 

## Bugfix
* mattermost: Update GetFileLinks to API_V4
* slack/mattermost: Fix issue with matterircd users not being able to chat to eachother
* slack: Do not join channels for single direct messages (slack)
* slack: Split fallback messages on newline (slack)

# v0.17.0
## New features
* general: mattermost configuration settings need to be migrated to `[mattermost]` settings. See matterircd.toml.example
* slack: Add BlackListUser config setting. Blacklist users from connecting to slack. See matterircd.toml.example
* slack: Add JoinMpImOnTalk config setting. Only join MultiPerson IM when someone talks. See mattericd.toml.example 
* slack: Add Restrict config setting. Only allowed to connect to specified slack teams. See matterircd.toml.example
* slack: Add UseDisplayName config setting. If displayname is set, the message will be prepended with `<displayname>`. See matterircd.toml.example
* slack: also show messages from bots
* slack: reconnect on disconnects

## Bugfix
* &users join speedup on teams with massive amount of users (tested on 26k users)
* Only allow 1 login/logout in progress
* slack: Fix on-join race condition
* slack: Always add yourself to your channels (fixes problem with > 500 users channels)
* slack: remove carriage returns from topic
* slack: Autojoin new channel/group when invited
* slack: Join MpIm channel if we haven't joined it yet

# v0.16.8
## Bugfix
* Fix newlines in topic to client. Closes 42wim/matterircd#163
* Remove double part messages. Closes 42wim/matterircd#156
* mattermost: Fix away (mattermost). Closes 42wim/matterircd#165
* slack: Look into attachments if message is empty (slack). Fixes 42wim/matterircd#160

# v0.16.7
## Bugfix
* mattermost: Update lastview every 60 second as antiIdle (mattermost). Closes 42wim/matterircd#147
* slack: Make sure we only can execute comands if login is fully done. Closes 42wim/matterircd#154
* slack: Add invite support (slack). Closes 42wim/matterircd#159
* slack: Add list support (slack)
* slack: Fix channel parts for private channels
* slack: Add kick support (slack)
* slack: Allow to mention yourself.. Closes 42wim/matterircd#157

# v0.16.6
## Bugfix
* Strip IRC colors. Closes 42wim/matterircd#149
* slack: Replace mentions, channels, etc..  #13
* slack: Use displayname if possible (instead of name)
* slack: Enable LinkNames. (@here,@all) Closes 42wim/matterircd#152
* slack: Add topic change support (slack). Closes 42wim/matterircd#151
* mattermost: Print list of valid team names when team not found (42wim/matterbridge#390)

# v0.16.5
## New features
* Add support for private channels in slack #142

## Bugfix
* Slack: fixes join/parts #143, #146
* Slack: fixes away #144

# v0.16.4
## Bugfix
* Fix some messages going to &messages #140

# v0.16.3
## Bugfix
* Fix crash on /nick change when not logged in #141

# v0.16.2
## Bugfix
* Remove crash on channel lookup of private messages

# v0.16.1
## Bugfix
* Remove debug code which could cause a crash
* Only append channel name to sender once in &messages

# v0.16.0
## New features
* `-conf` option (for a config file). See https://github.com/42wim/matterircd/blob/master/matterircd.toml.example for an example. Thanks @slowbro for this PR.
* New config file options
   * JoinExclude: an array of channels that won't be joined on IRC.
    Messages that get sent to unjoined channels (but you're joined on mattermost) will
    get sent to the &messages channel.
    You can still /JOIN exclude channels.

    JoinExclude = ["#town-square","#boringchannel"]

    * JoinInclude: an array of channels that only will be joined on IRC.
    If it's empty, it means all channels get joined (except those defined in JoinExclude)
    Messages that get sent to unjoined channels (but you're joined on mattermost) will
    get sent to the &messages channel.

    JoinInclude = ["#devops"]

    * PartFake: a bool that defines if you do a /LEAVE or /PART on IRC it will also
    actually leave the channel on mattermost.
    Default false

    PartFake = true

* don't log passwords used with 'mattermost' and 'slack'. Closes #73

## Bugfix
* Already read messages are replayed again and again #130
* Update to latest mattermost (4.6) libs
* Deprecated flags `-bindinterface` and `-port` removed

# v0.15.0
## New features
* Support mattermost 4.2 and higher (4.x) (use mattermost v4 API)
* Add -mmskiptlsverify option to skip TLS certificate checks on mattermost

## Enhancements
* Display nickname, if set #120
* Replace IRC parsing function with shellwords like function to allow for passwords with spaces. (#8)

# v0.14.0
## New features
* Support mattermost 4.1
* Add initial support for slack

# v0.13.0
## New features
* Support mattermost 4.0

## Enhancement
* Show slack attachments if they have a fallback/contain text
* Show links even when public links are disabled #105
* Edited messages now have "(edited)" appended
* ```-bind ""``` now disables non-tls port-binding when you have ```-tlsbind``` specified #109

## Bugfix
* Long messages from mattermost will be split in multiple smaller messages #103
* Fix join/leave messages for recent mattermost versions #113, #104
* Ignore messages sent to &users #108 
* Ignore posts that have a reaction (emoji) added #111


# v0.12.0
(thanks to @recht matterircd fork)
## New features
* Add KICK support
* Add INVITE support
* Also relay edited messages

## Enhancement
* Show the original message/author after replied messages
* Print timestamp of replayed messages
* Faster startup (joining channels)

## Bugfix
* Do not clear topic on empty /TOPIC command
* Fix various possible panics

# v0.11.6
## New features
* Support mattermost 3.10.0

# v0.11.5
## Bugfix
* Fix crash #97

# v0.11.4
## New features
* Support mattermost 3.9.0
## Enhancement
* Use props instead of ZWSP to check if message comes from matterircd (no more spaces after messages from matterircd -> mattermost)

# v0.11.3
## New features
* Support mattermost 3.7.0 and 3.8.0
## Bugfix
* Make public links (pasted images/attachments) work again

## New features
* Add support for public links in SCROLLBACK and SEARCH

# v0.11.2
## Bugfix
* Use correct status for /WHO #81
* Fix ISON for misbehaving clients #78

## New features
* Support mattermost 3.6.0

# v0.11.1
## Bugfix
* Fix crash on new channel joins #77
* Fix crash on channel join/leaves of other mattermost users #77

# v0.11.0
## New features
* Support mattermost 3.5.0
