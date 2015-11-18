package main

import (
	"flag"
	"fmt"
	"github.com/42wim/mm-go-irckit"
	"github.com/alexcesaro/log"
	"github.com/alexcesaro/log/golog"
	"net"
	"os"
)

var logger log.Logger = log.NullLogger

func main() {
	flagDebug := flag.Bool("debug", false, "enable debug logging")
	flagBindInterface := flag.String("interface", "127.0.0.1", "interface to bind to")
	flagBindPort := flag.Int("port", 6667, "Port to bind to")
	flag.Parse()

	logger = golog.New(os.Stderr, log.Info)
	if *flagDebug {
		logger.Info("enabling debug")
		logger = golog.New(os.Stderr, log.Debug)
	}

	irckit.SetLogger(logger)
	socket, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *flagBindInterface, *flagBindPort))
	if err != nil {
		logger.Errorf("Failed to listen on socket: %v\n", err)
	}
	defer socket.Close()

	start(socket)
}

func start(socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			logger.Errorf("Failed to accept connection: %v", err)
			return
		}

		go func() {
			newsrv := irckit.NewServer("matterircd")
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = newsrv.Connect(irckit.NewUserMM(conn, newsrv))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
