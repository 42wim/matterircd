# matrixclient

Small Matrix client library for matterircd/matterbridge, modelled on
[matterclient](https://github.com/matterbridge/matterclient). Wraps mautrix:
password login, sync loop, room/DM/user tracking, send (incl. threads),
and message/reaction callbacks through the `Handler` interface.
