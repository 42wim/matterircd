package irckit

import "strings"

// IRC 99-color (mIRC 00-98) palettes for deterministic thread context coloring.
//
// Grayscales/black/white (00, 01, 14, 15, 88-98) and exact RGB duplicates
// (52=04, 54=08, 58=11, 62=13) are omitted from every palette.
//
// Dark and light exclusions follow the curated unwanted color sets from
// matterircd_complete.pl (mapped onto the standard IRC 99-color palette),
// additionally excluding IRC 99 entries with poor contrast against dark or
// light/Solarized Light backgrounds.
var (
	// threadColorsDark excludes dark blues (02, 24, 36, 48, 60, 72, 84) and
	// very dark entries (05, 12, 16-29, 35-39, 49) that fade on dark themes.
	threadColorsDark = []string{
		"03", "04", "06", "07", "08", "09", "10", "11", "13",
		"30", "31", "32", "33", "34",
		"40", "41", "42", "43", "44", "45", "46", "47", "50", "51",
		"53", "55", "56", "57", "59", "61", "63",
		"64", "65", "66", "67", "68", "69", "70", "71", "73", "74", "75",
		"76", "77", "78", "79", "80", "81", "82", "83", "85", "86", "87",
	}

	// threadColorsLight excludes bright yellows/greens/cyans and pale pastels
	// (07-09, 11, 42-43, 45-46, 53-58, 65-71, 74, 76-87) that wash out on
	// light and Solarized Light backgrounds.
	threadColorsLight = []string{
		"02", "03", "04", "05", "06", "10", "12", "13",
		"16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27",
		"28", "29", "30", "31", "32", "33", "34", "35", "36", "37", "38", "39",
		"40", "41", "44", "47", "48", "49", "50", "51",
		"59", "60", "61", "63",
		"64", "72", "73", "75",
	}

	// threadColorsAuto is the intersection of threadColorsDark and
	// threadColorsLight, keeping mid-tone colors legible on both dark and
	// light backgrounds.
	threadColorsAuto = []string{
		"03", "04", "06", "10", "13",
		"30", "31", "32", "33", "34",
		"40", "41", "44", "47", "50", "51",
		"59", "61", "63",
		"64", "73", "75",
	}
)

const (
	threadColorModeDark  = "dark"
	threadColorModeLight = "light"
	threadColorModeAuto  = "auto"
	threadColorModeOn    = "on"
)

func threadColorPalette(mode string) []string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case threadColorModeDark:
		return threadColorsDark
	case threadColorModeLight:
		return threadColorsLight
	case threadColorModeAuto, threadColorModeOn:
		return threadColorsAuto
	default:
		return nil
	}
}

func fnv1a32(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)

	var h uint32 = offset32
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= prime32
	}

	return h
}

func threadContextColor(mode, id string) string {
	if id == "" {
		return ""
	}

	palette := threadColorPalette(mode)
	if len(palette) == 0 {
		return ""
	}

	return palette[int(fnv1a32(id)>>1)%len(palette)]
}

func formatSingleThreadContext(mode, prefix, id, hashID string) string {
	color := threadContextColor(mode, hashID)
	if color == "" {
		return "[" + prefix + id + "]"
	}

	return "\x03" + color + "[" + prefix + id + "]\x03"
}

func formatDualThreadContext(mode, prefix, parentTag, postTag, parentHashID, postHashID string) string {
	parentColor := threadContextColor(mode, parentHashID)
	if parentColor == "" {
		return "[" + prefix + parentTag + "," + postTag + "]"
	}

	postColor := threadContextColor(mode, postHashID)
	if postColor == "" || postColor == parentColor {
		return "\x03" + parentColor + "[" + prefix + parentTag + "," + postTag + "]\x03"
	}

	return "\x03" + parentColor + "[" + prefix + parentTag + ",\x03" + postColor + postTag + "\x03" + parentColor + "]\x03"
}

func colorizeLastSentLine(mode, line string) string {
	if !strings.HasPrefix(line, "[@@") {
		return line
	}

	closeIdx := strings.IndexByte(line, ']')
	if closeIdx <= 3 {
		return line
	}

	color := threadContextColor(mode, line[3:closeIdx])
	if color == "" {
		return line
	}

	return "\x03" + color + line[:closeIdx+1] + "\x03" + line[closeIdx+1:]
}
