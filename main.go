package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/42wim/mm-go-irckit"
	"github.com/Sirupsen/logrus"
	"net"
	"os"
	"strings"
)

var flagRestrict, flagDefaultTeam, flagDefaultServer, flagTLSBind, flagTLSDir *string
var flagInsecure *bool
var version = "0.11.2-dev"
var logger *logrus.Entry

func main() {
	flagDebug := flag.Bool("debug", false, "enable debug logging")
	flagBind := flag.String("bind", "127.0.0.1:6667", "interface:port to bind to.")
	flagBindInterface := flag.String("interface", "", "interface to bind to (deprecated: use -bind)")
	flagBindPort := flag.Int("port", 0, "Port to bind to (deprecated: use -bind)")
	flagRestrict = flag.String("restrict", "", "only allow connection to specified mattermost server/instances. Space delimited")
	flagDefaultTeam = flag.String("mmteam", "", "specify default mattermost team")
	flagDefaultServer = flag.String("mmserver", "", "specify default mattermost server/instance")
	flagInsecure = flag.Bool("mminsecure", false, "use http connection to mattermost")
	flagVersion := flag.Bool("version", false, "show version")
	flagTLSBind = flag.String("tlsbind", "", "interface:port to bind to. (e.g 127.0.0.1:6697)")
	flagTLSDir = flag.String("tlsdir", ".", "directory to look for key.pem and cert.pem.")
	flag.Parse()

	ourlog := logrus.New()
	ourlog.Formatter = &logrus.TextFormatter{FullTimestamp: true}
	logger = ourlog.WithFields(logrus.Fields{"module": "matterircd"})

	if *flagDebug {
		logger.Info("enabling debug")
		ourlog.Level = logrus.DebugLevel
		irckit.SetLogLevel("debug")
	}

	if *flagVersion {
		fmt.Println("Version:", version)
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
	// backwards compatible
	if *flagBind == "127.0.0.1:6667" && *flagBindInterface != "" && *flagBindPort != 0 {
		*flagBind = fmt.Sprintf("%s:%d", *flagBindInterface, *flagBindPort)
	}
	socket, err := net.Listen("tcp", *flagBind)
	if err != nil {
		logger.Errorf("Can not listen on %s: %v", *flagBind, err)
	}
	logger.Infof("Listening on %s", *flagBind)
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
			newsrv := irckit.ServerConfig{Name: "matterircd", Version: version}.Server()
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = newsrv.Connect(irckit.NewUserMM(conn, newsrv, cfg))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
