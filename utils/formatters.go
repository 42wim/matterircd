package utils

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
)

//nolint:funlen,gocyclo
func FormatCodeBlockText(text string, prefix string, codeBlockBackTick bool, codeBlockTilde bool, lexer string, syntaxHighlighting string, linePrefix string) (string, bool, bool, string) {
	if linePrefix != "" {
		unq, err := strconv.Unquote(`"` + linePrefix + `"`)
		if err == nil {
			linePrefix = unq
		}
	}

	// skip empty lines for anything not part of a code block.
	if text == "" {
		if codeBlockBackTick || codeBlockTilde {
			return linePrefix + " ", codeBlockBackTick, codeBlockTilde, lexer
		}
		return "", codeBlockBackTick, codeBlockTilde, lexer
	}

	if (strings.HasPrefix(text, "```") || strings.HasPrefix(text, prefix+"```")) && !codeBlockTilde {
		codeBlockBackTick = !codeBlockBackTick
		newText := ""
		if codeBlockBackTick {
			lexer = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "```"), prefix+"```"))
			if lexer != "" {
				newText = strings.Replace(text, "```", linePrefix, 1)
				newText = strings.Replace(newText, lexer, "\x16"+lexer+"\x16", 1)
			}
		}
		return newText, codeBlockBackTick, codeBlockTilde, lexer
	}
	if (strings.HasPrefix(text, "~~~") || strings.HasPrefix(text, prefix+"~~~")) && !codeBlockBackTick {
		codeBlockTilde = !codeBlockTilde
		newText := ""
		if codeBlockTilde {
			lexer = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "~~~"), prefix+"~~~"))
			if lexer != "" {
				newText = strings.Replace(text, "~~~", linePrefix, 1)
				newText = strings.Replace(newText, lexer, "\x16"+lexer+"\x16", 1)
			}
		}
		return newText, codeBlockBackTick, codeBlockTilde, lexer
	}

	if !(codeBlockBackTick || codeBlockTilde) {
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	if syntaxHighlighting == "" || lexer == "" {
		return linePrefix + text, codeBlockBackTick, codeBlockTilde, lexer
	}

	formatter := "terminal256"
	style := "pygments"
	v := strings.SplitN(syntaxHighlighting, ":", 2)
	if len(v) == 2 {
		formatter = v[0]
		style = v[1]
	}

	var b bytes.Buffer
	err := quick.Highlight(&b, text, lexer, formatter, style)
	if err == nil {
		text = linePrefix + b.String()
		// Work around https://github.com/alecthomas/chroma/issues/716
		text = strings.ReplaceAll(text, "\n", "")
	}

	return text, codeBlockBackTick, codeBlockTilde, lexer
}

// Use static initialisation to optimize.
// Bold & Italic - https://www.markdownguide.org/basic-syntax#bold-and-italic
var boldItalicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*\*\*)+?(.+?)(?:\*\*\*)+?`),
	regexp.MustCompile(`\b(?:\_\_\_)+?(.+?)(?:\_\_\_)+?\b`),
	regexp.MustCompile(`\b(?:\_\_\*)+?(.+?)(?:\*\_\_)+?\b`),
	regexp.MustCompile(`\b(?:\*\*\_)+?(.+?)(?:\_\*\*)+?\b`),
}

// Bold - https://www.markdownguide.org/basic-syntax#bold
var boldRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*\*)+?(.+?)(?:\*\*)+?`),
	regexp.MustCompile(`\b(?:\_\_)+?(.+?)(?:\_\_)+?\b`),
}

// Italic - https://www.markdownguide.org/basic-syntax#italic
var italicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*)+?([^\*]+?)(?:\*)+?`),
	regexp.MustCompile(`\b(?:\_)+?([^_]+?)(?:\_)+?\b`),
}

// Code / Monospace - https://markdownguide.offshoot.io/basic-syntax/#code
var codeRegExp = []*regexp.Regexp{
	regexp.MustCompile("(?:`)+?([^`]+?)(?:`)+?"),
}

func Markdown2irc(msg string, prefixChar string) string {
	// Bold & Italic 0x02+0x1d
	for _, re := range boldItalicRegExp {
		msg = re.ReplaceAllString(msg, "\x02\x1d$1\x1d\x02")
	}

	// Bold 0x02
	for _, re := range boldRegExp {
		msg = re.ReplaceAllString(msg, "\x02$1\x02")
	}

	// Italic 0x1d
	for _, re := range italicRegExp {
		msg = re.ReplaceAllString(msg, "\x1d$1\x1d")
	}

	// Code / Monospace 0x11
	for _, re := range codeRegExp {
		msg = re.ReplaceAllString(msg, "\x11\x030,14$1\x03\x11")
	}

	// Block quotes
	if strings.HasPrefix(msg, ">") {
		msg = strings.Replace(msg, ">", prefixChar, 1)
	}

	return msg
}
