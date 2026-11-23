# googlechatclient

Small Google Chat client library for matterircd/matterbridge, modelled on
[matterclient](https://github.com/matterbridge/matterclient),
[msteamsclient](https://github.com/matterbridge/msteamsclient),
[matrixclient](https://github.com/matterbridge/matrixclient),
[slackclient](https://github.com/matterbridge/slackclient), and
[rocketchatclient](https://github.com/matterbridge/rocketchatclient).
Supports both Google Workspace and personal (`@gmail.com`) accounts via browser
session cookie / `SAPISIDHASH` / XSRF authentication, WebChannel real-time
streaming (`webchannel/events_encoded`), and Dynamite protobuf RPC endpoints for
modern inline-threaded Spaces, 1:1 DMs, and Group DMs.
