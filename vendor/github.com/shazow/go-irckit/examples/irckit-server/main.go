package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"

	"github.com/alexcesaro/log"
	"github.com/alexcesaro/log/golog"
	"github.com/jessevdk/go-flags"

	"github.com/shazow/go-irckit"
)
import _ "net/http/pprof"

// version gets replaced during build
var version string = "dev"

// logger gets replaced by golog
var logger log.Logger = log.NullLogger

// Options contains the flag options
type Options struct {
	Bind    string `long:"bind" description:"Bind address to listen on." value-name:"[HOST]:PORT" default:":6667"`
	Pprof   string `long:"pprof" description:"Bind address to serve pprof for profiling." value-name:"[HOST]:PORT"`
	Name    string `long:"name" description:"Server name." default:"irckit-demo"`
	Motd    string `long:"motd" description:"Message of the day."`
	Verbose []bool `short:"v" long:"verbose" description:"Show verbose logging."`
	Version bool   `long:"version"`
}

var logLevels = []log.Level{
	log.Warning,
	log.Info,
	log.Debug,
}

func fail(code int, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(code)
}

func main() {
	options := Options{}
	parser := flags.NewParser(&options, flags.Default)
	_, err := parser.Parse()
	if err != nil {
		os.Exit(1)
		return
	}

	if options.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	if options.Pprof != "" {
		go func() {
			fmt.Println(http.ListenAndServe(options.Pprof, nil))
		}()
	}

	// Figure out the log level
	numVerbose := len(options.Verbose)
	if numVerbose > len(logLevels) {
		numVerbose = len(logLevels) - 1
	}

	logLevel := logLevels[numVerbose]
	logger = golog.New(os.Stderr, logLevel)
	irckit.SetLogger(logger)

	socket, err := net.Listen("tcp", options.Bind)
	if err != nil {
		fail(4, "Failed to listen on socket: %v\n", err)
	}
	defer socket.Close()

	motd := []string{}
	if options.Motd != "" {
		motd = append(motd, options.Motd)
	}
	srv := irckit.ServerConfig{
		Name: options.Name,
		Motd: motd,
	}.Server()
	go start(srv, socket)

	fmt.Printf("Listening for connections on %v\n", socket.Addr().String())

	// Construct interrupt handler
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	<-sig // Wait for ^C signal
	fmt.Fprintln(os.Stderr, "Interrupt signal detected, shutting down.")
	srv.Close()
	os.Exit(0)
}

func start(srv irckit.Server, socket net.Listener) {
	for {
		conn, err := socket.Accept()
		if err != nil {
			logger.Errorf("Failed to accept connection: %v", err)
			return
		}

		// Goroutineify to resume accepting sockets early
		go func() {
			logger.Infof("New connection: %s", conn.RemoteAddr())
			err = srv.Connect(irckit.NewUserNet(conn))
			if err != nil {
				logger.Errorf("Failed to join: %v", err)
				return
			}
		}()
	}
}
