package irckit

import "regexp"

func stringInRegexp(a string, list []string) bool {
	for _, entry := range list {
		re := regexp.MustCompile(entry)
		if re.MatchString(a) {
			return true
		}
	}

	return false
}

func removeStringInSlice(a string, list []string) []string {
	newlist := []string{}
	for _, b := range list {
		if b != a {
			newlist = append(newlist, b)
		}
	}
	return newlist
}
