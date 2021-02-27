module github.com/42wim/matterircd

require (
	github.com/davecgh/go-spew v1.1.1
	github.com/desertbit/timer v0.0.0-20180107155436-c41aec40b27f
	github.com/google/gops v0.3.11
	github.com/gorilla/websocket v1.4.2
	github.com/hashicorp/golang-lru v0.5.4
	github.com/jpillora/backoff v1.0.0
	github.com/kr/pretty v0.2.0 // indirect
	github.com/matterbridge/logrus-prefixed-formatter v0.5.3-0.20200523233437-d971309a77ba
	github.com/mattermost/mattermost-server/v5 v5.25.2
	github.com/mgutz/ansi v0.0.0-20170206155736-9520e82c474b // indirect
	github.com/mitchellh/mapstructure v1.2.3
	github.com/muesli/reflow v0.1.0
	github.com/onsi/gomega v1.5.0 // indirect
	github.com/sirupsen/logrus v1.6.0
	github.com/slack-go/slack v0.8.1
	github.com/sorcix/irc v1.1.4
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.7.0
	github.com/stretchr/testify v1.6.1
	github.com/x-cray/logrus-prefixed-formatter v0.5.2 // indirect
	golang.org/x/crypto v0.0.0-20200728195943-123391ffb6de // indirect
	golang.org/x/tools v0.0.0-20200529172331-a64b76657301 // indirect
	gopkg.in/yaml.v3 v3.0.0-20200605160147-a5ece683394c // indirect
)

//replace github.com/nlopes/slack v0.6.0 => github.com/matterbridge/slack v0.1.1-0.20191208194820-95190f11bfb6
//replace github.com/42wim/matterbridge => ../matterbridge-client

go 1.13
