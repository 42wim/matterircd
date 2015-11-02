# matterircd

Minimal IRC server which integrates with [mattermost](https://www.mattermost.org) 

Work in progress, does the basics for now. 

Most of the work happens in [mm-go-irckit](https://github.com/42wim/mm-go-irckit) (based on github.com/shazow/go-irckit)

# Usage

Matterircd will listen on localhost port 6667.  
Connect with your favorite irc-client to localhost:6667

1. Login to your favorite mattermost service by sending a /QUERY to mattermost
```
LOGIN <mattermost hostname> <teamname> <login> <pass>
```
![login](http://snag.gy/aAop5.jpg)

2. Join to e.g. #town-square (/JOIN #town-square)
![channel](http://snag.gy/IzlXR.jpg)

3. Chat away or join other channels
![chat](http://snag.gy/JyFd7.jpg)
![mmchat](http://snag.gy/3qMd1.jpg)


