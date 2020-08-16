module github.com/42wim/matterircd

require (
	github.com/42wim/matterbridge v1.18.1-0.20200809222954-4e50fd864921
	github.com/davecgh/go-spew v1.1.1
	github.com/desertbit/timer v0.0.0-20180107155436-c41aec40b27f
	github.com/google/gops v0.3.10
	github.com/kr/pretty v0.2.0 // indirect
	github.com/mattermost/mattermost-server/v5 v5.25.2
	github.com/mitchellh/mapstructure v1.2.3
	github.com/muesli/reflow v0.1.0
	github.com/onsi/gomega v1.5.0 // indirect
	github.com/sirupsen/logrus v1.6.0
	github.com/slack-go/slack v0.6.5
	github.com/sorcix/irc v1.1.4
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.7.0
	github.com/stretchr/testify v1.6.1
	golang.org/x/crypto v0.0.0-20200728195943-123391ffb6de // indirect
	gopkg.in/yaml.v3 v3.0.0-20200605160147-a5ece683394c // indirect
)

//replace github.com/nlopes/slack v0.6.0 => github.com/matterbridge/slack v0.1.1-0.20191208194820-95190f11bfb6
//replace github.com/42wim/matterbridge => ../matterbridge-client

go 1.13
