package main

import (
	"context"
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
	"sync"
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

	gopsRunning bool
	gopsMu      sync.Mutex

	profServer *http.Server
	profMu     sync.Mutex
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

	// Setup live-reloading subsystems
	setupLogReloadHook(cfg)
	setupProfilingReloadHook(cfg)
	setupGopsReloadHook(cfg)
	setupSignalHandling(cfg)

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

func setupLogReloadHook(cfg *config.Config) {
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

	setLogLevels(cfg.Current())
	cfg.RegisterReloadHook(setLogLevels)
}

func setupProfilingReloadHook(cfg *config.Config) {
	var (
		profServer *http.Server
		profMu     sync.Mutex
	)

	setProfiling := func(rc *config.RuntimeConfig) {
		profMu.Lock()
		defer profMu.Unlock()

		if rc.Profiling {
			if profServer == nil {
				logger.Info("enabling profiling: starting HTTP server: *:6060")
				runtime.SetBlockProfileRate(1)
				runtime.SetMutexProfileFraction(10)

				h := http.DefaultServeMux
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Del("Content-Encoding")
					h.ServeHTTP(w, r)
				})

				profServer = &http.Server{
					Addr:    ":6060",
					Handler: handler,
				}

				go func() {
					if err := profServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						logger.WithError(err).Error("profiling: Failed to start HTTP server")
					}
				}()
			}
		} else {
			if profServer != nil {
				logger.Info("disabling profiling: shutting down HTTP server: *:6060")
				runtime.SetBlockProfileRate(0)
				runtime.SetMutexProfileFraction(0)

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				if err := profServer.Shutdown(ctx); err != nil {
					logger.WithError(err).Error("profiling: Failed to gracefully shutdown")
				}
				profServer = nil
			}
		}
	}

	setProfiling(cfg.Current())
	cfg.RegisterReloadHook(setProfiling)
}

func setupGopsReloadHook(cfg *config.Config) {
	var (
		gopsRunning bool
		gopsMu      sync.Mutex
	)

	setGops := func(rc *config.RuntimeConfig) {
		gopsMu.Lock()
		defer gopsMu.Unlock()

		if rc.Gops {
			if !gopsRunning {
				logger.Info("enabling gops agent")
				if err := agent.Listen(agent.Options{}); err != nil {
					logger.WithError(err).Error("failed to start gops agent")
				} else {
					gopsRunning = true
				}
			}
		} else {
			if gopsRunning {
				logger.Info("disabling gops agent")
				agent.Close()
				gopsRunning = false
			}
		}
	}

	setGops(cfg.Current())
	cfg.RegisterReloadHook(setGops)
}

func setupSignalHandling(cfg *config.Config) {
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
