<!-- TOC -->

- [prefixcontext](#prefixcontext)
    - [view edit/delete of other users](#view-editdelete-of-other-users)
    - [view reactions](#view-reactions)
    - [view threads](#view-threads)
    - [reply to threads](#reply-to-threads)
    - [quote in reply](#quote-in-reply)
    - [modify/delete your own messages](#modifydelete-your-own-messages)
    - [add/remove reactions to messages/posts from other users](#addremove-reactions-to-messagesposts-from-other-users)

<!-- /TOC -->

# prefixcontext

When enabling this you'll get a hex number between [000] and [fff] prefixed to each message.
Every channel/direct message will have a separate counter.

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

## quote in reply

You can quote an existing message and include your reply underneath it using a unified `>` quote prefix:

- `>@@<id> <message>` — always posts the quote as a top-level (unthreaded) message in the channel or DM.
- `@@<id> >@ <message>` — quotes `<id>` and posts according to the bridge's `QuoteThreaded` setting.
- `@@<thread_id> >@@<post_id> <message>` — replies inside `<thread_id>` while quoting `<post_id>` (can reference a different message or thread).

All forms support `!!` (the last message you sent), 3-hex context IDs (`001`), and native bridge message IDs.

For Markdown-quoted bridges (Mattermost and Slack), you can optionally limit how much of the original message is quoted by placing `<N><L|W|C>` (case-insensitive, `N >= 1`) immediately after `>`:

- `L` / `l`: first `N` lines (e.g. `>5l@@001 msg`, `@@001 >5l@ msg`, `@@001 >5l@@002 msg`)
- `W` / `w`: first `N` words (e.g. `>6w@@001 msg`, `@@001 >6w@ msg`)
- `C` / `c`: first `N` bytes, extending to the next word boundary so words/emojis are not split mid-word (e.g. `>10c@@001 msg`, `@@001 >10c@ msg`)

When truncated, an ellipsis (`...`, or `…` when `Unicode = true`) is appended. Bridges with native quote cards (Google Chat, Matrix, Microsoft Teams, and Rocket.Chat) ignore the length modifier and send their native quote attachment instead.

```irc
21:55 <wim> [001] Original message content here
21:56 <wimtest> @@001 >@ My message follows it
21:57 <wimtest> >@@001 Quoting this back into the main channel
21:58 <wimtest> >5l@@001 Quoting only the first 5 lines
21:59 <wimtest> @@001 >6w@@002 Replying in thread 001 while quoting 6 words of 002
22:00 <wimtest> @@!! >@ Following up on my last message
```

On Markdown-quoted bridges (Mattermost and Slack), this sends a blockquote followed by your message:

```markdown
> **wim** wrote:
> Original message content here

My message follows it
```

Per-bridge configuration options in `matterircd.toml`:

- `QuoteThreaded`: Controls whether `@@<id> >@ <message>` replies inside the original message's thread (defaults to `true`, except `googlechat` which defaults to `false`). `>@@<id> <message>` always sends unthreaded, and `@@<thread_id> >@@<post_id> <message>` always replies inside `<thread_id>`.
- `QuoteAuthor`: Controls author attribution in Markdown quotes (Mattermost and Slack): `"displayname"` (default), `"nick"`, or `"off"` / `"none"` to omit the author header. Ignored by bridges that send native quote cards (Google Chat, Matrix, Microsoft Teams, and Rocket.Chat).

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

## add/remove reactions to messages/posts from other users

To add a reaction to a message/post, just use +:reaction: as follows:

```irc
23:25 <@wim> [001] hi
23:25 < wimirc> hello how are you
23:25 <@wim> [003] fine
23:25 < wimirc> @@003 +:thumbsup:
23:25 < wimirc> added reaction: thumbsup
```

You can also remove reactions.

```irc
23:25 <@wim> [001] hi
23:25 < wimirc> hello how are you
23:25 <@wim> [003] fine
23:25 < wimirc> @@003 +:thumbsup:
23:25 < wimirc> added reaction: thumbsup
23:25 < wimirc> @@003 -:thumbsup:
23:25 < wimirc> removed reaction: thumbsup
```
