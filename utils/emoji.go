package utils

import (
	"slices"
	"strings"
	"sync"

	"github.com/kenshaw/emoji"
)

type SkinTone rune

const (
	SkinToneNeutral     SkinTone = 0
	SkinToneLight       SkinTone = 0x1f3fb
	SkinToneMediumLight SkinTone = 0x1f3fc
	SkinToneMedium      SkinTone = 0x1f3fd
	SkinToneMediumDark  SkinTone = 0x1f3fe
	SkinToneDark        SkinTone = 0x1f3ff
)

var skinToneSuffixes = []struct {
	suffix string
	tone   SkinTone
}{
	{"_light_skin_tone", SkinToneLight},
	{"_medium_light_skin_tone", SkinToneMediumLight},
	{"_medium_skin_tone", SkinToneMedium},
	{"_medium_dark_skin_tone", SkinToneMediumDark},
	{"_dark_skin_tone", SkinToneDark},
	{"_light", SkinToneLight},
	{"_medium_light", SkinToneMediumLight},
	{"_medium", SkinToneMedium},
	{"_medium_dark", SkinToneMediumDark},
	{"_dark", SkinToneDark},
}

// defaultAliases maps custom/legacy shortcodes to target emoji keys or Unicode characters.
var defaultAliases = map[string]string{
	"alert":                         "rotating_light",
	"checkmark":                     "white_check_mark",
	"done2":                         "white_check_mark",
	"jenkins_fire":                  "fire",
	"rolling_on_the_floor_laughing": "rofl",
	"shipit":                        "chipmunk",
}

// GetEmojiMap initializes and returns the immutable alias-to-Unicode map once using Go 1.21+ sync.OnceValue.
var GetEmojiMap = sync.OnceValue(func() map[string]string {
	data := emoji.Gemoji()
	aliasMap := make(map[string]string, (len(data)*3)+len(defaultAliases))

	register := func(name string, unicodeVal string, supportsSkinTone bool) {
		if name == "" {
			return
		}

		// But only if it doesn't already exist, e.g. "angry"
		if _, exists := aliasMap[name]; !exists {
			aliasMap[name] = unicodeVal
		}

		if !supportsSkinTone {
			return
		}

		// Support skin tones
		for _, st := range skinToneSuffixes {
			variantKey := name + st.suffix
			if _, exists := aliasMap[variantKey]; !exists {
				aliasMap[variantKey] = applySkinTone(unicodeVal, st.tone)
			}
		}
	}

	for _, e := range data {
		if e.Emoji == "" {
			continue
		}

		for _, alias := range e.Aliases {
			register(alias, e.Emoji, e.SkinTones)
		}

		// In addition to emoji aliases, include emoji tags
		for _, tag := range e.Tags {
			register(tag, e.Emoji, e.SkinTones)
		}
	}

	// Register built-in aliases
	for alias, target := range defaultAliases {
		targetClean := trimColons(target)
		// If target resolves to an existing emoji, alias it
		if unicodeVal, exists := aliasMap[targetClean]; exists {
			if _, taken := aliasMap[alias]; !taken {
				aliasMap[alias] = unicodeVal
			}
		}
	}

	return aliasMap
})

// EmojiReplaceAliases scans the input and replaces :alias: tokens with Unicode emojis.
//
//nolint:funlen
func EmojiReplaceAliases(s string, customAliases map[string]string) string {
	if strings.IndexByte(s, ':') < 0 {
		return s
	}

	emojiMap := GetEmojiMap()

	var (
		b           strings.Builder
		initialized bool
		lastWrite   int
		start       = -1
	)

	for i := range len(s) {
		ch := s[i]

		// Emojis never span across spaces or control characters
		if ch <= ' ' {
			start = -1

			continue
		}

		// Handle normal characters outside of colons
		if ch != ':' {
			continue
		}

		// We found a colon, mark it as the start of a potential emoji
		if start == -1 {
			start = i

			continue
		}

		// Empty pair "::" — treat current index as a new candidate start
		if i == start+1 {
			start = i

			continue
		}

		// We found a second colon, test the substring
		inner := s[start+1 : i]

		if emojiStr, ok := lookupEmoji(inner, emojiMap, customAliases); ok {
			if !initialized {
				b.Grow(len(s))

				initialized = true
			}

			// Write the text between the last write and the start of this emoji
			b.WriteString(s[lastWrite:start])
			// Write the actual emoji
			b.WriteString(emojiStr)

			lastWrite = i + 1
			start = -1 // Reset for the next emoji
		} else {
			// Not a valid emoji (e.g., a timestamp "12:30:00").
			// Treat this current colon as the new start in case it opens an emoji.
			start = i
		}
	}

	// If we never found a valid emoji, return the original string with zero allocations.
	if !initialized {
		return s
	}

	// Write any remaining text after the last replaced emoji
	b.WriteString(s[lastWrite:])

	return b.String()
}

// EmojiFromAlias looks up an emoji by shortcode, supporting with or without surrounding colons.
func EmojiFromAlias(alias string, customAliases map[string]string) (string, bool) {
	if alias == "" {
		return "", false
	}

	cleaned := trimColons(alias)
	if cleaned == "" {
		return "", false
	}

	return lookupEmoji(cleaned, GetEmojiMap(), customAliases)
}

// applySkinTone places the Fitzpatrick modifier in the correct Unicode sequence position.
func applySkinTone(baseEmoji string, tone SkinTone) string {
	if tone == SkinToneNeutral || baseEmoji == "" {
		return baseEmoji
	}

	runes := []rune(baseEmoji)
	if len(runes) == 0 {
		return baseEmoji
	}

	// If the emoji already contains any Fitzpatrick modifier, do not double-tone it
	for _, r := range runes {
		if r >= 0x1F3FB && r <= 0x1F3FF {
			return baseEmoji
		}
	}

	insertAt := len(runes)

	// Complex ZWJ sequence (e.g. 🙇‍♂️): insert skin tone before the first ZWJ (0x200D)
	if zwjIdx := slices.Index(runes, 0x200D); zwjIdx != -1 {
		insertAt = zwjIdx
	}

	// Standard emoji: insert before trailing presentation selector \uFE0F if present
	if insertAt > 0 && runes[insertAt-1] == 0xFE0F {
		insertAt--
	}

	return string(slices.Insert(runes, insertAt, rune(tone)))
}

func lookupEmoji(alias string, emojiMap map[string]string, customAliases map[string]string) (string, bool) {
	if len(customAliases) > 0 {
		if mapped, ok := customAliases[alias]; ok {
			alias = trimColons(mapped)
		}
	}

	val, ok := emojiMap[alias]

	return val, ok
}

func trimColons(s string) string {
	start := 0
	end := len(s)

	for start < end && s[start] == ':' {
		start++
	}

	for end > start && s[end-1] == ':' {
		end--
	}

	return s[start:end]
}
