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
