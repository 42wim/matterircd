package main

import (
	"github.com/42wim/mm-go-irckit"
	"github.com/alexcesaro/log"
	"github.com/alexcesaro/log/golog"
	"net"
	"os"
)

var logger log.Logger = log.NullLogger

func main() {
	logger = golog.New(os.Stderr, log.Debug)
	irckit.SetLogger(logger)

	socket, err := net.Listen("tcp", "127.0.0.1:6667")
	if err != nil {
		logger.Errorf("Failed to listen on socket: %v\n", err)
	}
	defer socket.Close()

	start(irckit.NewServer("matterircd"), socket)
}

func start(srv irckit.Server, socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			logger.Errorf("Failed to accept connection: %v", err)
			return
		}

		go func() {
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = srv.Connect(irckit.NewUserMM(conn, srv))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
