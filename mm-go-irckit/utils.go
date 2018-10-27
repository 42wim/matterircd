package irckit

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
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
