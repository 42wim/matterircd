package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/config"
	irckit "github.com/42wim/matterircd/mm-go-irckit"
	"github.com/google/gops/agent"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	_ "net/http/pprof" //nolint:gosec
)

var (
	version = "0.31.0-dev"
	githash string
	logger  *logrus.Entry
	cfg     *config.Config

	LastViewedSaveDB *bolt.DB
)

func main() {
	ourlog := logrus.New()
	ourlog.Formatter = &prefixed.TextFormatter{
		PrefixPadding: 11,
		DisableColors: false,
		FullTimestamp: true,
	}
	logger = ourlog.WithFields(logrus.Fields{"prefix": "matterircd"})
	config.SetLogger(logger)

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
	var err error
	cfg, err = config.Load(*flagConfig, pflag.CommandLine)
	if err != nil {
		log.Fatal(err)
	}
	rc := cfg.Current()

	// Helper function to set log levels on startup and reloads
	setLogLevels := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			logger.Info("enabling trace")
			config.SetLogLevel(logrus.TraceLevel)
			irckit.SetLogLevel("trace")
		case rc.Debug:
			logger.Info("enabling debug")
			config.SetLogLevel(logrus.DebugLevel)
			irckit.SetLogLevel("debug")
		default:
			// Fallback to Info when Debug/Trace are toggled off live
			config.SetLogLevel(logrus.InfoLevel)
			irckit.SetLogLevel("info")
		}
	}

	// Set initial log level at startup
	setLogLevels(rc)

	// Register hook to update log levels dynamically on config reload
	cfg.RegisterReloadHook(setLogLevels)

	// Setup SIGHUP listener for manual live reloads via `kill -s HUP <PID>`
	sighupChan := make(chan os.Signal, 1)
	signal.Notify(sighupChan, syscall.SIGHUP)

	go func() {
		for range sighupChan {
			logger.Info("received SIGHUP signal, triggering config reload...")
			if err := cfg.Reload(); err != nil {
				logger.WithError(err).Error("SIGHUP config reload failed")
			}
		}
	}()

	if rc.Gops {
		if err := agent.Listen(agent.Options{}); err != nil {
			log.Fatal(err)
		}
	}

	if rc.Profiling {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(10)
		go func() {
			logger.Infof("enabling profiling: start HTTP server: *:6060")

			h := http.DefaultServeMux
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Remove any gzip (or other content-encoding) header.
				w.Header().Del("Content-Encoding")
				h.ServeHTTP(w, r)
			})

			if err := http.ListenAndServe(":6060", handler); err != nil { //nolint:gosec
				logger.Fatal("profiling: Failed to start HTTP server", err)
			}
		}()
	}

	if flag.Lookup("version").Value.String() == "true" {
		fmt.Printf("version: %s %s\n", version, githash)
		return
	}

	irckit.SetLogger(logger)

	logger.Infof("Running version %s %s", version, githash)
	if strings.Contains(version, "-dev") {
		logger.Infof("WARNING: THIS IS A DEVELOPMENT VERSION. Things may break.")
	}

	if rc.TLSBind != "" {
		go func() {
			logger.Infof("Listening on %s (TLS)", rc.TLSBind)
			socket := tlsbind()
			defer socket.Close()
			start(socket)
		}()
	}

	mmLastViewedFile := "matterircd-lastsaved.db"
	if statePath := rc.Mattermost.LastViewedSaveFile; statePath != "" {
		mmLastViewedFile = statePath
	}
	db, err := bolt.Open(mmLastViewedFile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()
	LastViewedSaveDB = db

	// backwards compatible

	if rc.Bind != "" {
		go func() {
			var network string
			if strings.ContainsRune(rc.Bind, os.PathSeparator) {
				network = "unix"
			} else {
				network = "tcp"
			}

			socket, err := net.Listen(network, rc.Bind)
			if err != nil {
				logger.Errorf("Can not listen on %s: %v", rc.Bind, err)
				os.Exit(1)
			}

			logger.Infof("Listening on %s", rc.Bind)

			defer socket.Close()
			start(socket)
		}()
	}

	select {}
}

func tlsbind() net.Listener {
	rc := cfg.Current()
	certPath := rc.TLSDir + "/cert.pem"
	keyPath := rc.TLSDir + "/key.pem"

	if rc.TLSCert != "" {
		certPath = rc.TLSCert
	}

	if rc.TLSKey != "" {
		keyPath = rc.TLSKey
	}

	kpr, err := NewKeypairReloader(certPath, keyPath)
	if err != nil {
		logger.Errorf("could not load TLS, incorrect directory? Error: %s", err)
		os.Exit(1)
	}

	tlsConfig := tls.Config{
		GetCertificate: kpr.GetCertificateFunc(),
	}

	listenerTLS, err := tls.Listen("tcp", rc.TLSBind, &tlsConfig)
	if err != nil {
		logger.Errorf("Can not listen on %s: %v\n", rc.TLSBind, err)
		os.Exit(1)
	}

	logger.Info("TLS listening on ", rc.TLSBind)

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

			user := irckit.NewUserBridge(conn, newsrv, cfg, LastViewedSaveDB)
			err = newsrv.Connect(user)
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
