FROM alpine:edge
ENTRYPOINT ["/bin/matterircd"]

COPY . /go/src/github.com/42wim/matterircd
RUN apk update && apk add go git \
        && cd /go/src/github.com/42wim/matterircd \
        && export GOPATH=/go \
        && go get \
        && go build -o /bin/matterircd \
        && rm -rf /go \
        && apk del --purge git go
