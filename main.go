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
	"github.com/42wim/matterircd/utils"
	"github.com/google/gops/agent"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	_ "net/http/pprof" //nolint:gosec
)

var (
	project = "matterircd"
	version = "0.33.0-dev"
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
	logger = ourlog.WithFields(logrus.Fields{"prefix": project})
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

	// profiling related cfg
	flag.String("profilingbind", "127.0.0.1:6060", "interface:port to bind the profiling server to.")

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	if flag.Lookup("version").Value.String() == "true" {
		fmt.Printf("%s %s %s\n", project, version, githash)
		return
	}

	logger.Infof("Running version %s %s", version, githash)
	if strings.Contains(version, "-dev") {
		logger.Infof("WARNING: THIS IS A DEVELOPMENT VERSION. Things may break.")
	}

	userAgent := project + "/" + version
	if githash != "" {
		userAgent = userAgent + " (" + githash + ")"
	}

	config.UserAgent = userAgent

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

	irckit.SetLogger(logger)

	// We bind before starting goroutines so port conflicts are caught instantly.
	var tlsSocket net.Listener
	if rc.TLSBind != "" {
		tlsSocket = tlsbind()
	}

	// Backwards compatible: Bind standard socket synchronously (if configured)
	var stdSocket net.Listener
	if rc.Bind != "" {
		var network string
		if strings.ContainsRune(rc.Bind, os.PathSeparator) {
			network = "unix"
		} else {
			network = "tcp"
		}

		var err error
		stdSocket, err = net.Listen(network, rc.Bind)
		if err != nil {
			logger.Fatal(err)
		}
	}

	// Now that ports are secured, open the database
	mmLastViewedFile := project + "-lastsaved.db"
	if statePath := rc.Mattermost.LastViewedSaveFile; statePath != "" {
		mmLastViewedFile = statePath
	}
	db, err := bolt.Open(mmLastViewedFile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		logger.Fatalf("Database lock failed (is another instance running?): %v", err)
	}
	defer db.Close()
	LastViewedSaveDB = db

	aiCfg := cfg.Current().AI
	if aiCfg.Enabled { //nolint:nestif
		provider := aiCfg.Provider
		if provider == "" {
			provider = "gemini"
		}

		model := aiCfg.GetModel(provider)

		switch strings.ToLower(provider) {
		case "copilot", "github":
			if aiCfg.Token == "" {
				logger.Warn("AI summarization enabled, but token is missing")
				break
			}

			_, err := utils.NewCopilotClient(aiCfg.Token, model)
			if err != nil {
				logger.Errorf("AI summarization setup error: %v", err)
				break
			}

			logger.Infof("AI summarization enabled (default provider: %s, model: %s)", provider, model)
		default:
			if aiCfg.ServiceAccountFile == "" {
				logger.Warn("AI summarization enabled, but service_account_file or project is missing")
				break
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := utils.NewGeminiClient(ctx, aiCfg.ServiceAccountFile, aiCfg.Project, aiCfg.Location, model)
			if err != nil {
				logger.Errorf("AI summarization setup error: %v", err)
				break
			}

			logger.Infof(
				"AI summarization enabled (default provider: %s, model: %s, project: %s, region: %s)",
				provider, model, aiCfg.Project, aiCfg.Location,
			)
		}
	} else {
		logger.Debug("AI summarization is disabled")
	}

	// Start serving connections asynchronously
	if tlsSocket != nil {
		logger.Infof("Listening on %s (TLS)", rc.TLSBind)
		go func() {
			defer tlsSocket.Close()
			start(tlsSocket)
		}()
	}

	// Backwards compatible
	if stdSocket != nil {
		logger.Infof("Listening on %s", rc.Bind)
		go func() {
			defer stdSocket.Close()
			start(stdSocket)
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

//nolint:funlen
func setupProfilingReloadHook(cfg *config.Config) {
	var (
		profServer *http.Server
		profMu     sync.Mutex
	)

	setProfiling := func(rc *config.RuntimeConfig) {
		profMu.Lock()
		defer profMu.Unlock()

		// Get the bind address, fallback to localhost if empty
		bindAddr := rc.ProfilingBind
		if bindAddr == "" {
			bindAddr = "127.0.0.1:6060"
		}

		if rc.Profiling { //nolint:nestif
			if profServer != nil && profServer.Addr != bindAddr {
				logger.Infof("profiling bind address changed, restarting server...")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := profServer.Shutdown(ctx); err != nil {
					logger.WithError(err).Error("profiling: Failed to gracefully shutdown old server")
				}
				profServer = nil
			}

			if profServer == nil {
				logger.Infof("enabling profiling: starting HTTP server: %s", bindAddr)
				runtime.SetBlockProfileRate(1)
				runtime.SetMutexProfileFraction(10)

				h := http.DefaultServeMux
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Del("Content-Encoding")
					h.ServeHTTP(w, r)
				})

				profServer = &http.Server{
					Addr:              bindAddr,
					Handler:           handler,
					ReadHeaderTimeout: 3 * time.Second,
				}

				go func() {
					if err := profServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						logger.WithError(err).Error("profiling: Failed to start HTTP server")
					}
				}()
			}
		} else if profServer != nil {
			logger.Infof("disabling profiling: shutting down HTTP server: %s", profServer.Addr)
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

		if rc.Gops { //nolint:nestif
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
		logger.Fatalf("could not load TLS, incorrect directory? Error: %s", err)
	}

	tlsConfig := tls.Config{
		GetCertificate: kpr.GetCertificateFunc(),
	}

	listenerTLS, err := tls.Listen("tcp", rc.TLSBind, &tlsConfig)
	if err != nil {
		logger.Fatal(err)
	}

	return listenerTLS
}

func start(socket net.Listener) {
	fullVersion := version
	if githash != "" {
		fullVersion = version + " (" + githash + ")"
	}

	for {
		conn, err := socket.Accept()
		if err != nil {
			logger.Errorf("Failed to accept connection: %v", err)
			return
		}

		go func() {
			newsrv := irckit.ServerConfig{Name: project, Version: fullVersion}.Server()

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
