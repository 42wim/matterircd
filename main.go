package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/mm-go-irckit"
	"github.com/sirupsen/logrus"
	"net"
	"os"
	"strings"
)

var (
	flagRestrict, flagDefaultTeam, flagDefaultServer, flagTLSBind, flagTLSDir *string
	flagInsecure, flagSkipTLSVerify                                           *bool
	version                                                                   = "0.18.3"
	githash                                                                   string
	logger                                                                    *logrus.Entry
	cfg                                                                       config.Config
)

func main() {
	ourlog := logrus.New()
	ourlog.Formatter = &logrus.TextFormatter{FullTimestamp: true}
	logger = ourlog.WithFields(logrus.Fields{"module": "matterircd"})
	config.Logger = logger

	// config related. instantiate a new config.Config to store flags
	cfg = config.Config{}
	flagConfig := flag.String("conf", "", "config file")

	// bools for showing version/enabling debug
	flagVersion := flag.Bool("version", false, "show version")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable debug logging")

	// bind related cfg
	flag.StringVar(&cfg.Bind, "bind", "127.0.0.1:6667", "interface:port to bind to.")

	// mattermost related cfg
	flag.StringVar(&cfg.Restrict, "restrict", "", "only allow connection to specified mattermost server/instances. Space delimited")
	flag.StringVar(&cfg.DefaultTeam, "mmteam", "", "specify default mattermost team")
	flag.StringVar(&cfg.DefaultServer, "mmserver", "", "specify default mattermost server/instance")
	flag.BoolVar(&cfg.Insecure, "mminsecure", false, "use http connection to mattermost")

	// TLS related cfg
	flag.BoolVar(&cfg.SkipTLSVerify, "mmskiptlsverify", false, "skip verification of mattermost certificate chain and hostname")
	flag.StringVar(&cfg.TLSBind, "tlsbind", "", "interface:port to bind to. (e.g 127.0.0.1:6697)")
	flag.StringVar(&cfg.TLSDir, "tlsdir", ".", "directory to look for key.pem and cert.pem.")
	flag.Parse()

	// if -config was set, load the config file (overrides args)
	if *flagConfig != "" {
		cfg = *config.LoadConfig(*flagConfig, cfg)
	}

	if cfg.Debug {
		logger.Info("enabling debug")
		ourlog.Level = logrus.DebugLevel
		irckit.SetLogLevel("debug")
	}
	if *flagVersion {
		fmt.Printf("version: %s %s\n", version, githash)
		return
	}

	irckit.SetLogger(logger)

	logger.Infof("Running version %s %s", version, githash)
	if strings.Contains(version, "-dev") {
		logger.Infof("WARNING: THIS IS A DEVELOPMENT VERSION. Things may break.")
	}

	if cfg.TLSBind != "" {
		go func() {
			logger.Infof("Listening on %s (TLS)", cfg.TLSBind)
			socket := tlsbind()
			defer socket.Close()
			start(socket)
		}()
	}

	// backwards compatible
	if cfg.Bind == "127.0.0.1:6667" && cfg.BindInterface != "" && cfg.BindPort != 0 {
		cfg.Bind = fmt.Sprintf("%s:%d", cfg.BindInterface, cfg.BindPort)
	}

	if cfg.Bind != "" {
		go func() {
			socket, err := net.Listen("tcp", cfg.Bind)
			if err != nil {
				logger.Errorf("Can not listen on %s: %v", cfg.Bind, err)
			}
			logger.Infof("Listening on %s", cfg.Bind)
			defer socket.Close()
			start(socket)
		}()
	}
	select {}
}

func tlsbind() net.Listener {
	cert, err := tls.LoadX509KeyPair(cfg.TLSDir+"/cert.pem", cfg.TLSDir+"/key.pem")
	if err != nil {
		logger.Errorf("could not load TLS, incorrect directory? Error: %s", err)
		os.Exit(1)
	}
	config := tls.Config{Certificates: []tls.Certificate{cert}}
	listenerTLS, err := tls.Listen("tcp", cfg.TLSBind, &config)
	if err != nil {
		logger.Errorf("Can not listen on %s: %v\n", cfg.TLSBind, err)
		os.Exit(1)
	}
	logger.Info("TLS listening on ", cfg.TLSBind)
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
			irccfg := &irckit.MmCfg{Insecure: cfg.Insecure, SkipTLSVerify: cfg.SkipTLSVerify,
				SlackSettings: cfg.Slack, MattermostSettings: cfg.Mattermost}
			newsrv := irckit.ServerConfig{Name: "matterircd", Version: version}.Server()
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = newsrv.Connect(irckit.NewUserMM(conn, newsrv, irccfg))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
