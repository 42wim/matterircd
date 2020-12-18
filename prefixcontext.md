<!-- TOC -->

- [prefixcontext](#prefixcontext)
    - [view edit/delete of other users](#view-editdelete-of-other-users)
    - [view reactions](#view-reactions)
    - [view threads](#view-threads)
    - [reply to threads](#reply-to-threads)
    - [modify/delete your own messages](#modifydelete-your-own-messages)

<!-- /TOC -->

# prefixcontext

When enabling this you'll get a hex number between [000] and [fff] prefixed to each message.
Every channel/direct message will have a seperate counter.

This way you can see what operation has happened on which message.

(Only support for mattermost right now)

## view edit/delete of other users

```irc
23:42 <@wim> [001] x
23:42 <@wim> [002] blah
23:42 <@wim> [002] blah blah (edited)
23:42 <@wim> [003] test
23:42 <@wim> [004] more
23:42 <@wim> [001] x (deleted)
```

In the message above you can see that `[002]` has been edited, and `[001]` has been deleted.

## view reactions

Now you can also see those reactions, in future versions this may become unicode.

```irc
01:20 <@wim> [005] test
01:20 <@wim> [006] another message
01:20 <@wim> [007] something else
01:20 <@wim> [006] added reaction: sunglasses
01:21 <@wim> [006] added reaction: money_mouth_face
01:21 <@wim> [005] added reaction: rofl
01:21 <@wim> [005] removed reaction: rofl
```

## view threads

You can also see who replied to what thread
[replynumber->threadnumber]

```irc
19:58 <@wim> [001] normal message
19:58 <@wim> [002] another one
19:58 <@wim> [003->001] in a thread
19:58 <@wim> [004->001] another message in same thread
19:58 <@wim> [005] normal message
19:59 <@wim> [006->002] new thread
19:59 <@wim> [002] another one changes (edited)
20:00 <@wim> [004] another message in same thread (deleted)
20:01 <@wim> [002] added reaction: money_mouth_face
```

## reply to threads

With `@@number` you can reply to a message and it'll be threaded in mattermost

```irc
21:55 <wim> [001] abc
21:56 <wim> [002] def
21:56 <wim> [003->002] lala
21:56 <wimtest> @@001 xyz
```

## modify/delete your own messages

With `s/number/newtext` you can replace your message with the new content.
You'll have to calculate the number yourself, in the example below it's `003`

To delete the message just set the newtext empty `s/number/`

```irc
23:25 <@wim> [001] hi
23:25 <@wim> [002] something
23:25 < wimirc> hllo how are you
23:25 < wimirc> s/003/hello how are you
23:25 <@wim> [004] fine
```

You can also modify or delete the last message you sent.

```irc
23:25 <@wim> [001] hi
23:25 <@wim> [002] something
23:25 < wimirc> hllo how are you
23:25 < wimirc> s//hello how are you
23:25 < wimirc> s/!!/hello, how are you?
23:25 <@wim> [004] fine
23:25 < wimirc> good
23:25 < wimirc> s//
```

Or start a thread with the last message you sent.

```irc
23:25 <@wim> [001] hi
23:25 <@wim> [002] something
23:25 < wimirc> hello
23:25 < wimirc> @@!! how are you?
23:25 <@wim> [005->003] good
```
