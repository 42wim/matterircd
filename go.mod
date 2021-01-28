module github.com/42wim/matterircd

require (
	github.com/42wim/matterbridge v1.21.0
	github.com/davecgh/go-spew v1.1.1
	github.com/desertbit/timer v0.0.0-20180107155436-c41aec40b27f
	github.com/google/gops v0.3.14
	github.com/gorilla/websocket v1.4.2
	github.com/hashicorp/golang-lru v0.5.4
	github.com/jpillora/backoff v1.0.0
	github.com/matterbridge/logrus-prefixed-formatter v0.5.3-0.20200523233437-d971309a77ba
	github.com/mattermost/mattermost-server/v5 v5.30.1
	github.com/mitchellh/mapstructure v1.3.3
	github.com/muesli/reflow v0.1.0
	github.com/sirupsen/logrus v1.7.0
	github.com/slack-go/slack v0.7.4
	github.com/sorcix/irc v1.1.4
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.7.1
	github.com/stretchr/testify v1.6.1
	maunium.net/go/mautrix v0.8.0
)

//replace github.com/nlopes/slack v0.6.0 => github.com/matterbridge/slack v0.1.1-0.20191208194820-95190f11bfb6
//replace github.com/42wim/matterbridge => ../matterbridge-client

go 1.13
