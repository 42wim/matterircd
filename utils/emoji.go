package utils

import (
	"strings"
	"sync"

	"github.com/kenshaw/emoji"
)

var (
	emojiInitOnce sync.Once
	emojiData     []emoji.Emoji
	emojiAliasMap map[string]int
)

type emojiSkinToneInfo struct {
	suffix string
	match  string
	tone   emoji.SkinTone
}

var emojiSkinTones = []emojiSkinToneInfo{
	{"light", "_light", emoji.Light},
	{"medium_light", "_medium_light", emoji.MediumLight},
	{"medium", "_medium", emoji.Medium},
	{"medium_dark", "_medium_dark", emoji.MediumDark},
	{"dark", "_dark", emoji.Dark},
}

func initEmoji() {
	data := emoji.Gemoji()

	emojiAliasMap = make(map[string]int, len(data))

	for i, e := range data {
		if e.Emoji == "" {
			continue
		}
		for _, alias := range e.Aliases {
			if alias == "" {
				continue
			}
			emojiAliasMap[alias] = i
		}
		// In addition to emoji aliases, include emoji tags
		for _, tag := range e.Tags {
			if tag == "" {
				continue
			}
			// But only if it doesn't already exist, e.g. "angry"
			if _, ok := emojiAliasMap[tag]; !ok {
				emojiAliasMap[tag] = i
			}
		}
	}

	emojiData = data
}

func EmojiReplaceAliases(s string, customAliases map[string]string) string {
	if strings.IndexByte(s, ':') < 0 {
		return s
	}

	emojiInitOnce.Do(initEmoji)

	var (
		b           strings.Builder
		initialized bool
		lastWrite   int
	)

	start := -1
	for i := range len(s) {
		// Handle normal characters outside of colons
		if s[i] != ':' {
			continue
		}

		// We found a colon, mark it as the start of a potential emoji
		if start == -1 {
			start = i
			continue
		}

		// We found a second colon, test the substring
		code := s[start : i+1]
		if emojiStr, ok := EmojiFromAlias(code, customAliases); ok {
			// This is our first confirmed emoji. Initialize the builder.
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

func EmojiFromAlias(alias string, customAliases map[string]string) (string, bool) {
	if alias == "" {
		return "", false
	}

	emojiInitOnce.Do(initEmoji)

	if a, ok := strings.CutPrefix(alias, ":"); ok {
		if a, ok := strings.CutSuffix(a, ":"); ok {
			alias = a
		}
	}

	// Helper to resolve custom aliases and optionally strip colons if the user configured `plus1=:+1:`
	resolveAlias := func(a string) string {
		if mapped, ok := customAliases[a]; ok {
			if trimmed, ok := strings.CutPrefix(mapped, ":"); ok {
				if trimmed, ok := strings.CutSuffix(trimmed, ":"); ok {
					return trimmed
				}
			}

			return mapped
		}

		return a
	}

	// Check if the exact alias was mapped (e.g. plus1 -> +1)
	lookupAlias := resolveAlias(alias)
	if idx, ok := emojiAliasMap[lookupAlias]; ok {
		return emojiData[idx].Emoji, true
	}

	// Skin tone support
	base, ok := strings.CutSuffix(lookupAlias, "_skin_tone")
	if !ok {
		return "", false
	}

	// Support skin tones
	for _, st := range emojiSkinTones {
		if baseAlias, ok := strings.CutSuffix(base, st.match); ok {
			// Check if the BASE was mapped (e.g., they mapped plus1=+1, and typed :plus1_dark_skin_tone:)
			baseAlias = resolveAlias(baseAlias)

			idx, mapOk := emojiAliasMap[baseAlias]
			if !mapOk {
				// "_light" matches the end of "_medium_light".
				// Continue to get the correct skin tone.
				continue
			}

			return emojiData[idx].Tone(st.tone), true
		}
	}

	return "", false
}
