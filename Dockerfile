FROM alpine:edge
ENTRYPOINT ["/bin/matterircd"]

COPY . /go/src/github.com/42wim/matterircd
RUN apk update && apk add go git gcc musl-dev ca-certificates \
        && cd /go/src/github.com/42wim/matterircd \
        && export GOPATH=/go \
        && go get \
        && CGO_ENABLED=1 go build -x -tags goolm -ldflags "-X main.githash=$(git log --pretty=format:'%h' -n 1)" -o /bin/matterircd \
        && rm -rf /go \
        && apk del --purge git go gcc musl-dev
