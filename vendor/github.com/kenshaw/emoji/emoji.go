// Package emoji provides standard tools for working with emoji unicode codes and aliases.
package emoji

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:generate go run gen.go

// Emoji represents a single emoji and associated data.
type Emoji struct {
	Emoji          string   `json:"emoji"`
	Description    string   `json:"description"`
	Category       string   `json:"category"`
	Aliases        []string `json:"aliases"`
	Tags           []string `json:"tags"`
	UnicodeVersion string   `json:"unicode_version"`
	IOSVersion     string   `json:"ios_version"`
	SkinTones      bool     `json:"skin_tones"`
}

// Format satisfies the [fmt.Formatter] interface.
func (emoji Emoji) Format(f fmt.State, verb rune) {
	switch verb {
	case 's', 'c':
		fmt.Fprint(f, emoji.Emoji)
	case 'v':
		if f.Flag('#') {
			fmt.Fprintf(
				f,
				`{%s, %q, %q, %#v, %#v, %q, %q, %t}`,
				strconv.QuoteToASCII(emoji.Emoji),
				emoji.Description,
				emoji.Category,
				emoji.Aliases,
				emoji.Tags,
				emoji.UnicodeVersion,
				emoji.IOSVersion,
				emoji.SkinTones,
			)
		} else {
			fmt.Fprint(f, emoji.Emoji)
		}
	}
}

// Tone applies a skin tone to the emoji, if applicable.
func (emoji Emoji) Tone(skinTone SkinTone) string {
	if !emoji.SkinTones || skinTone == Neutral {
		return emoji.Emoji
	}
	st, r := string(skinTone), []rune(emoji.Emoji)
	return string(r[:1]) + st + manWomanRE.ReplaceAllString(string(r[1:]), `$1`+st)
}

// manWomanRE is regexp that matches the man, woman emojis.
var manWomanRE = regexp.MustCompile(`(👨|👩)`)

// SkinTone is a skin tone modifier.
type SkinTone rune

// Skin tone values.
const (
	Neutral     SkinTone = 0
	Light       SkinTone = 0x1f3fb
	MediumLight SkinTone = 0x1f3fc
	Medium      SkinTone = 0x1f3fd
	MediumDark  SkinTone = 0x1f3fe
	Dark        SkinTone = 0x1f3ff
)

// String satisfies the [fmt.Stringer] interface.
func (s SkinTone) String() string {
	switch s {
	case Neutral:
		return "neutral"
	case Light:
		return "light"
	case MediumLight:
		return "medium-light"
	case Medium:
		return "medium"
	case MediumDark:
		return "medium-dark"
	case Dark:
		return "dark"
	}
	return fmt.Sprintf("SkinTone<%x>", rune(s))
}

var (
	// codeMap provides a map of the emoji unicode code to its emoji data.
	codeMap map[string]int
	// aliasMap provides a map of the alias to its emoji data.
	aliasMap map[string]int
	// codeReplacer is the string replacer for emoji codes.
	codeReplacer *strings.Replacer
	// aliasReplacer is the string replacer for emoji aliases.
	aliasReplacer *strings.Replacer
	// aliasEmoticonReplacer is the string replacer for emoji aliases with
	// emoticons.
	aliasEmoticonReplacer *strings.Replacer
	// emoticonRE is the regexp to match emoticons on word boundaries.
	emoticonRE *regexp.Regexp
	// emoticonCodeMap is the map of emoticons to their emoji value.
	emoticonCodeMap map[string]string
	// emoticonCodeMap is the map of emoticons to their emoji alias.
	emoticonAliasMap map[string]string
)

func init() {
	data := Gemoji()
	// initialize
	codeMap = make(map[string]int, len(data))
	aliasMap = make(map[string]int, len(data))
	emoticonCodeMap = make(map[string]string)
	emoticonAliasMap = make(map[string]string)
	// process emoji codes and aliases
	var codePairs, aliasPairs []string
	for i, e := range data {
		if e.Emoji == "" || len(e.Aliases) == 0 {
			continue
		}
		// codes and aliases
		codeMap[e.Emoji], codePairs = i, append(codePairs, e.Emoji, ":"+e.Aliases[0]+":")
		for _, a := range e.Aliases {
			if a == "" {
				continue
			}
			aliasMap[a], aliasPairs = i, append(aliasPairs, ":"+a+":", e.Emoji)
		}
	}
	// process emoticons
	reVals := make([]string, 0)
	aliasEmoticonPairs := make([]string, 0)
	for a, vals := range emoticonMap {
		alias := ":" + a + ":"
		aliasEmoticonPairs = append(aliasEmoticonPairs, alias, vals[0])
		for _, u := range vals {
			reVals = append(reVals, regexp.QuoteMeta(u))
			emoticonCodeMap[u] = data[aliasMap[a]].Emoji
			emoticonAliasMap[u] = alias
		}
	}
	// create emoticon regexp
	emoticonRE = regexp.MustCompile(`(?m:^|\A|\s|\B)(` + strings.Join(reVals, "|") + `)(?:$|\z|\s)`)
	// create replacers
	codeReplacer = strings.NewReplacer(codePairs...)
	aliasReplacer = strings.NewReplacer(aliasPairs...)
	aliasEmoticonReplacer = strings.NewReplacer(aliasEmoticonPairs...)
}

// FromCode retrieves the emoji data based on the provided unicode code (ie,
// "\u2618" will return the Gemoji data for "shamrock").
func FromCode(code string) *Emoji {
	i, ok := codeMap[code]
	if !ok {
		return nil
	}
	data := Gemoji()
	return &data[i]
}

// FromAlias retrieves the emoji data based on the provided alias in the form
// "alias" or ":alias:" (ie, "shamrock" or ":shamrock:" will return the Gemoji
// data for "shamrock").
func FromAlias(alias string) *Emoji {
	if strings.HasPrefix(alias, ":") && strings.HasSuffix(alias, ":") {
		alias = alias[1 : len(alias)-1]
	}
	i, ok := aliasMap[alias]
	if !ok {
		return nil
	}
	data := Gemoji()
	return &data[i]
}

// FromEmoticon retrieves the emoji data based on the provided emoticon (ie,
// ":o)" will return the Gemoji data for "monkey face").
func FromEmoticon(emoticon string) *Emoji {
	alias, ok := emoticonAliasMap[emoticon]
	if !ok {
		return nil
	}
	return FromAlias(alias)
}

// ReplaceCodes replaces all emoji codes with the first corresponding emoji
// alias (in the form of ":alias:") (ie, "\u2618" will be converted to
// ":shamrock:").
func ReplaceCodes(s string) string {
	return codeReplacer.Replace(s)
}

// ReplaceAliases replaces all aliases of the form ":alias:" with its
// corresponding unicode value.
func ReplaceAliases(s string) string {
	return aliasReplacer.Replace(s)
}

// emoticonReplacer replaces all matched emoticon strings in s with the its
// corresponding map'd value in repl.
func emoticonReplacer(s string, repl map[string]string) string {
	matches := emoticonRE.FindAllStringSubmatchIndex(s, -1)
	// bail if no matches
	if len(matches) == 0 {
		return s
	}
	// build replacement string
	var buf bytes.Buffer
	last := 0
	for _, m := range matches {
		buf.WriteString(s[last:m[2]])
		e, ok := repl[s[m[2]:m[3]]]
		if !ok {
			panic("could not find emoticon!!")
		}
		buf.WriteString(e)
		last = m[3]
	}
	buf.WriteString(s[last:])
	return buf.String()
}

// ReplaceEmoticonsWithCodes replaces all emoticons (ie, :D, :p, etc) with the
// corresponding emoji code (ie, the monkey face emoticon ":o)" will be
// replaced with "\U0001f435").
func ReplaceEmoticonsWithCodes(s string) string {
	return emoticonReplacer(s, emoticonCodeMap)
}

// ReplaceEmoticonsWithAliases replaces all emoticons (ie, :D, :p, etc) with
// the first corresponding emoji alias (in the form of :alias:) (ie, the monkey
// face emoticon ":o)" will be replaced with ":monkey_face:").
func ReplaceEmoticonsWithAliases(s string) string {
	return emoticonReplacer(s, emoticonAliasMap)
}

// ReplaceAliasesWithEmoticons replaces all emoji aliases (in the form of
// :alias:) with its corresponding emoticon (ie, :D, :p, etc) (ie, the alias
// ":monkey_face:" will be replaced with ":o)").
func ReplaceAliasesWithEmoticons(s string) string {
	return aliasEmoticonReplacer.Replace(s)
}

// emoticonMap is a map of emoji aliases to emoticon counterparts.
var emoticonMap = map[string][]string{
	"angry":                        {">:(", ">:-("},
	"anguished":                    {"D:"},
	"broken_heart":                 {"</3"},
	"confused":                     {":/", ":-/", `:\`, `:-\`},
	"disappointed":                 {":(", "):", ":-("},
	"heart":                        {"<3"},
	"kiss":                         {":*", ":-*"},
	"laughing":                     {":>", ":->"},
	"monkey_face":                  {":o)"},
	"neutral_face":                 {":|"},
	"open_mouth":                   {":o", ":O", ":-o", ":-O"},
	"slightly_smiling_face":        {":)", "(:", ":-)"},
	"smile":                        {":D", ":-D"},
	"smiley":                       {"=)", "=-)"},
	"stuck_out_tongue":             {":p", ":P", ":-p", ":-P", ":b", ":-b"},
	"stuck_out_tongue_winking_eye": {";p", ";P", ";-p", ";-P", ";b", ";-b"},
	"sunglasses":                   {"8)"},
	"wink":                         {";)", ";-)"},
}
