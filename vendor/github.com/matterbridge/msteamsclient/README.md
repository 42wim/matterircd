# msteamsclient

Small Microsoft Teams client library for matterircd/matterbridge, modelled on
[matterclient](https://github.com/matterbridge/matterclient),
[matrixclient](https://github.com/matterbridge/matrixclient), and
[slackclient](https://github.com/matterbridge/slackclient). Supports both
Work/School (Entra ID) and Personal (MSA / Consumer Teams) accounts via
OAuth2 Device Code / refresh token / skypetoken auth, Trouter v4 real-time
events, and `chatsvc` / CSA / Middle-Tier / Presence REST endpoints.
