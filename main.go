package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/42wim/mm-go-irckit"
	"github.com/alexcesaro/log"
	"github.com/alexcesaro/log/golog"
	"net"
	"os"
	"strings"
)

var logger log.Logger = log.NullLogger
var flagRestrict, flagDefaultTeam, flagDefaultServer, flagTLSBind, flagTLSDir *string
var flagInsecure *bool
var Version = "0.4-dev"

func main() {
	flagDebug := flag.Bool("debug", false, "enable debug logging")
	flagBindInterface := flag.String("interface", "127.0.0.1", "interface to bind to")
	flagBindPort := flag.Int("port", 6667, "Port to bind to")
	flagRestrict = flag.String("restrict", "", "only allow connection to specified mattermost server/instances. Space delimited")
	flagDefaultTeam = flag.String("mmteam", "", "specify default mattermost team")
	flagDefaultServer = flag.String("mmserver", "", "specify default mattermost server/instance")
	flagInsecure = flag.Bool("mminsecure", false, "use http connection to mattermost")
	flagVersion := flag.Bool("version", false, "show version")
	flagTLSBind = flag.String("tlsbind", "", "interface:port to bind to. (e.g 127.0.0.1:6697)")
	flagTLSDir = flag.String("tlsdir", ".", "directory to look for key.pem and cert.pem.")
	flag.Parse()

	logger = golog.New(os.Stderr, log.Info)
	if *flagDebug {
		logger.Info("enabling debug")
		logger = golog.New(os.Stderr, log.Debug)
	}
	if *flagVersion {
		fmt.Println("Version:", Version)
		return
	}

	irckit.SetLogger(logger)
	if *flagTLSBind != "" {
		go func() {
			socket := tlsbind()
			defer socket.Close()
			start(socket)
		}()
	}
	socket, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *flagBindInterface, *flagBindPort))
	if err != nil {
		logger.Errorf("Can not listen on %s: %v\n", *flagBindInterface, err)
	}
	defer socket.Close()
	start(socket)
}

func tlsbind() net.Listener {
	cert, err := tls.LoadX509KeyPair(*flagTLSDir+"/cert.pem", *flagTLSDir+"/key.pem")
	if err != nil {
		logger.Errorf("could not load TLS, incorrect directory?")
		os.Exit(1)
	}
	config := tls.Config{Certificates: []tls.Certificate{cert}}
	listenerTLS, err := tls.Listen("tcp", *flagTLSBind, &config)
	if err != nil {
		logger.Errorf("Can not listen on %s: %v\n", *flagTLSBind, err)
		os.Exit(1)
	}
	logger.Info("TLS listening on", *flagTLSBind)
	return listenerTLS
}

func start(socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			logger.Errorf("Failed to accept connection: %v", err)
			return
		}

		go func() {
			cfg := &irckit.MmCfg{AllowedServers: strings.Fields(*flagRestrict),
				DefaultTeam: *flagDefaultTeam, DefaultServer: *flagDefaultServer,
				Insecure: *flagInsecure}
			newsrv := irckit.ServerConfig{Name: "matterircd", Version: Version}.Server()
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = newsrv.Connect(irckit.NewUserMM(conn, newsrv, cfg))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
