# About emoji

`emoji` provides standardized ways for translating unicode code points for
Emoji to/from their [emoji cheat sheet][emoji-cheat-sheet] encoding, and is
most useful when working with third-party APIs such as Slack, GitHub, etc.

`emoji` was written because other emoji packages for Go only provided cheat
sheet names to unicode conversion and not the inverse. Additionally, there were
no comprehensive [emoticon][wiki-emoticon] packages available at the time.

[![Unit Tests][emoji-ci-status]][emoji-ci]
[![Go Reference][goref-emoji-status]][goref-emoji]
[![Releases][release-status]][Releases]
[![Discord Discussion][discord-status]][discord]

[emoji-ci]: https://github.com/kenshaw/emoji/actions/workflows/test.yml "Test CI"
[emoji-ci-status]: https://github.com/kenshaw/emoji/actions/workflows/test.yml/badge.svg "Test CI"
[goref-emoji]: https://pkg.go.dev/github.com/kenshaw/emoji "Go Reference"
[goref-emoji-status]: https://pkg.go.dev/badge/github.com/kenshaw/emoji.svg "Go Reference"
[release-status]: https://img.shields.io/github/v/release/kenshaw/emoji?display_name=tag&sort=semver "Latest Release"
[discord]: https://discord.gg/WDWAgXwJqN "Discord Discussion"
[discord-status]: https://img.shields.io/discord/829150509658013727.svg?label=Discord&logo=Discord&colorB=7289da&style=flat-square "Discord Discussion"
[releases]: https://github.com/kenshaw/emoji/releases "Releases"

## Installing

Install in the usual [Go][go-project] fashion:

```sh
$ go get github.com/kenshaw/emoji@latest
```

## Using

`emoji` can be used similarly to the following:

```go
// _example/example.go
package main

import (
	"log"

	"github.com/kenshaw/emoji"
)

func main() {
	a := emoji.FromEmoticon(":-)")
	log.Printf(":-) %+v", a)

	b := emoji.FromAlias("slightly_smiling_face")
	log.Printf(":-) %+v", b)

	s := emoji.ReplaceEmoticonsWithAliases(":-) :D >:(")
	log.Printf("s: %s", s)

	n := emoji.ReplaceEmoticonsWithCodes(":-) :D >:(")
	log.Printf("n: %s", n)
}
```

Please see the [Go Reference][goref-emoji] listing for the complete API listing.

## Gemoji Data

Data for this package is generated from GitHub's [`gemoji`][gemoji] project:

```sh
$ cd $GOPATH/src/github.com/kenshaw/emoji
$ go generate
```

## TODO

- Convert `UnicodeVersion` and `IOSVersion` fields of `Emoji` type to something
  more easily comparable (ie, int)

## Related Projects

- [`github.com/kenshaw/wofimoji`][wofimoji] - a rofi/wofi emoji picker

[emoji-cheat-sheet]: http://www.webpagefx.com/tools/emoji-cheat-sheet/
[gemoji]: https://github.com/github/gemoji
[go-project]: https://golang.org/project
[wofimoji]: https://github.com/kenshaw/wofimoji
[wiki-emoticon]: https://en.wikipedia.org/wiki/Emoticon
