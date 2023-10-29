# About emoji

`emoji` provides standardized ways for translating unicode code points for
Emoji to/from their [emoji cheat sheet][emoji-cheat-sheet] encoding, and is
most useful when working with third-party APIs such as Slack, GitHub, etc.

`emoji` was written because other emoji packages for Go only provided cheat
sheet names to unicode conversion and not the inverse. Additionally, there were
no comprehensive [emoticon][wiki-emoticon] packages available at the time.

## Gemoji Data

Data for this package is generated from GitHub's [gemoji][gemoji] project:

```sh
$ cd $GOPATH/src/github.com/kenshaw/emoji
$ go generate
```

## Installing

Install in the usual [Go][go-project] fashion:

```sh
$ go get -u github.com/kenshaw/emoji
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

Please see the [GoDoc][godoc] listing for the complete API listing.

## TODO

* Convert `UnicodeVersion` and `IOSVersion` fields of `Emoji` type to something
  more easily comparable (ie, int)

[emoji-cheat-sheet]: http://www.webpagefx.com/tools/emoji-cheat-sheet/
[gemoji]: https://github.com/github/gemoji
[godoc]: https://godoc.org/github.com/kenshaw/emoji
[go-project]: https://golang.org/project
[wiki-emoticon]: https://en.wikipedia.org/wiki/Emoticon
