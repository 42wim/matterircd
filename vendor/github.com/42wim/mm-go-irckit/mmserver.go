package irckit

func (s *server) Logout(user *User) {
	channels := user.Channels()
	for _, ch := range channels {
		for _, other := range ch.Users() {
			s.Lock()
			delete(s.users, other.ID())
			s.Unlock()
		}
		ch.Part(user, "")
		ch.Unlink()
	}
}

func (s *server) ChannelCount() int {
	return len(s.channels)
}
func (s *server) UserCount() int {
	return len(s.users)
}
