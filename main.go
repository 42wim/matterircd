package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/config"
	irckit "github.com/42wim/matterircd/mm-go-irckit"
	"github.com/google/gops/agent"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	version = "0.24.3-dev"
	githash string
	logger  *logrus.Entry
	v       *viper.Viper

	LastViewedSaveDB *bolt.DB
)

func main() {
	ourlog := logrus.New()
	ourlog.Formatter = &logrus.TextFormatter{FullTimestamp: true}
	logger = ourlog.WithFields(logrus.Fields{"module": "matterircd"})
	config.Logger = logger

	// config related. instantiate a new config.Config to store flags
	flagConfig := flag.String("conf", "matterircd.toml", "config file")

	// bools for showing version/enabling debug
	flag.Bool("version", false, "show version")
	flag.Bool("debug", false, "enable debug logging")

	// bind related cfg
	flag.String("bind", "127.0.0.1:6667", "interface:port to bind to, or a path to bind to a Unix socket.")

	// TLS related cfg
	flag.String("tlsbind", "", "interface:port to bind to. (e.g 127.0.0.1:6697)")
	flag.String("tlsdir", ".", "directory to look for key.pem and cert.pem.")

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	// Attempt to load values from the config file
	if _, err := os.Stat(*flagConfig); err == nil {
		v, err = config.LoadConfig(*flagConfig)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		v = viper.New()
	}

	v.BindPFlags(pflag.CommandLine)

	if v.GetBool("debug") {
		logger.Info("enabling debug")
		ourlog.Level = logrus.DebugLevel
		irckit.SetLogLevel("debug")
	}

	if v.GetBool("trace") {
		logger.Info("enabling trace")
		ourlog.Level = logrus.TraceLevel
		irckit.SetLogLevel("trace")
	}

	if v.GetBool("gops") {
		if err := agent.Listen(agent.Options{}); err != nil {
			log.Fatal(err)
		}
	}

	if v.GetBool("version") {
		fmt.Printf("version: %s %s\n", version, githash)
		return
	}

	irckit.SetLogger(logger)

	logger.Infof("Running version %s %s", version, githash)
	if strings.Contains(version, "-dev") {
		logger.Infof("WARNING: THIS IS A DEVELOPMENT VERSION. Things may break.")
	}

	if v.GetString("tlsbind") != "" {
		go func() {
			logger.Infof("Listening on %s (TLS)", v.GetString("tlsbind"))
			socket := tlsbind()
			defer socket.Close()
			start(socket)
		}()
	}

	mmLastViewedFile := "matterircd-lastsaved.db"
	if statePath := v.GetString("mattermost.LastViewedSaveFile"); statePath != "" {
		mmLastViewedFile = statePath
	}
	db, err := bolt.Open(mmLastViewedFile, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()
	LastViewedSaveDB = db

	// backwards compatible

	if v.GetString("bind") != "" {
		go func() {
			var network string
			if strings.ContainsRune(v.GetString("bind"), os.PathSeparator) {
				network = "unix"
			} else {
				network = "tcp"
			}

			socket, err := net.Listen(network, v.GetString("bind"))
			if err != nil {
				logger.Errorf("Can not listen on %s: %v", v.GetString("bind"), err)
				os.Exit(1)
			}

			logger.Infof("Listening on %s", v.GetString("bind"))

			defer socket.Close()
			start(socket)
		}()
	}

	select {}
}

func tlsbind() net.Listener {
	certPath := v.GetString("tlsdir") + "/cert.pem"
	keyPath := v.GetString("tlsdir") + "/key.pem"

	if v.GetString("tlscert") != "" {
		certPath = v.GetString("tlscert")
	}

	if v.GetString("tlskey") != "" {
		keyPath = v.GetString("tlskey")
	}

	kpr, err := NewKeypairReloader(certPath, keyPath)
	if err != nil {
		logger.Errorf("could not load TLS, incorrect directory? Error: %s", err)
		os.Exit(1)
	}

	tlsConfig := tls.Config{
		GetCertificate: kpr.GetCertificateFunc(),
	}

	listenerTLS, err := tls.Listen("tcp", v.GetString("tlsbind"), &tlsConfig)
	if err != nil {
		logger.Errorf("Can not listen on %s: %v\n", v.GetString("tlsbind"), err)
		os.Exit(1)
	}

	logger.Info("TLS listening on ", v.GetString("tlsbind"))

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
			newsrv := irckit.ServerConfig{Name: "matterircd", Version: version}.Server()

			logger.Infof("New connection: %s", conn.RemoteAddr())

			user := irckit.NewUserBridge(conn, newsrv, v, LastViewedSaveDB)
			err = newsrv.Connect(user)
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
