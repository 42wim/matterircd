package utils

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
)

//nolint:funlen,gocyclo
func FormatCodeBlockText(text string, codeBlockBackTick bool, codeBlockTilde bool, lexer string, syntaxHighlighting string, linePrefix string) (string, bool, bool, string) {
	if linePrefix != "" {
		if unq, err := strconv.Unquote(`"` + linePrefix + `"`); err == nil {
			linePrefix = unq
		}
	}

	trimmedText := strings.TrimLeft(text, " \t")

	handleToggle := func(prefix string, isActive bool) string {
		if isActive {
			newLexer := strings.TrimSpace(strings.TrimPrefix(trimmedText, prefix))
			if newLexer != "" {
				lexer = newLexer
				return linePrefix + "\x16" + lexer + "\x16"
			}
			return ""
		}
		lexer = ""
		return ""
	}

	if strings.HasPrefix(trimmedText, "```") && !codeBlockTilde {
		codeBlockBackTick = !codeBlockBackTick
		return handleToggle("```", codeBlockBackTick), codeBlockBackTick, codeBlockTilde, lexer
	}

	if strings.HasPrefix(trimmedText, "~~~") && !codeBlockBackTick {
		codeBlockTilde = !codeBlockTilde
		return handleToggle("~~~", codeBlockTilde), codeBlockBackTick, codeBlockTilde, lexer
	}

	codeBlock := codeBlockBackTick || codeBlockTilde
	if !codeBlock {
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	var sb strings.Builder
	sb.WriteString(linePrefix)

	if text == "" {
		sb.WriteByte(' ')
		return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
	}

	if syntaxHighlighting == "" || lexer == "" {
		sb.WriteString(text)
		return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
	}

	formatter := "terminal256"
	style := "pygments"
	if idx := strings.IndexByte(syntaxHighlighting, ':'); idx >= 0 {
		formatter = syntaxHighlighting[:idx]
		style = syntaxHighlighting[idx+1:]
	}

	var b bytes.Buffer
	if err := quick.Highlight(&b, text, lexer, formatter, style); err == nil {
		bs := b.Bytes()
		// Work around https://github.com/alecthomas/chroma/issues/716
		const resetSeq = "\x1b[0m"
		hasReset := bytes.HasSuffix(bs, []byte(resetSeq))
		if hasReset {
			bs = bs[:len(bs)-len(resetSeq)]
		}
		if len(bs) > 0 && bs[len(bs)-1] == '\n' {
			bs = bs[:len(bs)-1]
		}
		if hasReset {
			bs = append(bs, resetSeq...)
		}

		sb.Write(bs)
	} else {
		sb.WriteString(text)
	}

	return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
}

// Use static initialisation to optimize.
// Bold & Italic - https://www.markdownguide.org/basic-syntax#bold-and-italic
var boldItalicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*\*\*([^\*]+)\*\*\*`),
	regexp.MustCompile(`\b\_\_\_([^\_]+)\_\_\_\b`),
}

// Bold - https://www.markdownguide.org/basic-syntax#bold
var boldRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*\*([^\*]+)\*\*`),
	regexp.MustCompile(`\b\_\_([^\_]+)\_\_\b`),
}

// Italic - https://www.markdownguide.org/basic-syntax#italic
var italicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*([^\*]+)\*`),
	regexp.MustCompile(`\b\_([^\_]+)\_\b`),
}

// Code / Monospace - https://markdownguide.offshoot.io/basic-syntax/#code
var codeRegExp = []*regexp.Regexp{
	regexp.MustCompile("`+([^`]+)`+"),
}

const blockQuoteCharDefault = ">"

func Markdown2irc(msg string, blockQuoteChar string, inlineCode string) string {
	if !strings.ContainsAny(msg, "*_`>") {
		return msg
	}

	// Bold & Italic 0x02+0x1d
	if strings.ContainsAny(msg, "*_") {
		for _, re := range boldItalicRegExp {
			msg = re.ReplaceAllString(msg, "\x02\x1d${1}\x1d\x02")
		}

		// Bold 0x02
		for _, re := range boldRegExp {
			msg = re.ReplaceAllString(msg, "\x02${1}\x02")
		}

		// Italic 0x1d
		for _, re := range italicRegExp {
			msg = re.ReplaceAllString(msg, "\x1d${1}\x1d")
		}
	}

	// Code / Monospace 0x11
	if strings.Contains(msg, "`") {
		if inlineCode != "" {
			if unq, err := strconv.Unquote(`"` + inlineCode + `"`); err == nil {
				inlineCode = unq
			}
		} else {
			inlineCode = "\x02\x030,14"
		}
		for _, re := range codeRegExp {
			// Not all IRC clients support monospace (0x11) so keep the fence
			msg = re.ReplaceAllString(msg, "\x0f`\x11"+inlineCode+"${1}"+"\x11\x0f`")
		}
	}

	// Block quotes
	trimmedText := strings.TrimLeft(msg, " \t")
	if strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
		if unq, err := strconv.Unquote(`"` + blockQuoteChar + `"`); err == nil {
			blockQuoteChar = unq
		}
		msg = strings.Replace(msg, blockQuoteCharDefault, blockQuoteChar, 1)
	}

	return msg
}
