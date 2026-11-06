package main

import (
	"crypto/tls"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type keypairReloader struct {
	certMu   sync.RWMutex
	cert     *tls.Certificate
	certPath string
	keyPath  string
}

func NewKeypairReloader(certPath, keyPath string) (*keypairReloader, error) {
	result := &keypairReloader{
		certPath: certPath,
		keyPath:  keyPath,
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath) //nolint:wsl_v5
	if err != nil {
		return nil, err
	}
	result.cert = &cert //nolint:wsl_v5
	go func() {         //nolint:wsl_v5
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		for range c { //nolint:wsl_v5
			log.Printf("Received SIGHUP, reloading TLS certificate and key from %q and %q", certPath, keyPath)

			err := result.maybeReload()
			if err != nil {
				log.Printf("Keeping old TLS certificate because the new one could not be loaded: %v", err)
			}
		}
	}()
	return result, nil //nolint:wsl_v5
}

func (kpr *keypairReloader) maybeReload() error {
	newCert, err := tls.LoadX509KeyPair(kpr.certPath, kpr.keyPath)
	if err != nil {
		return err
	}
	kpr.certMu.Lock() //nolint:wsl_v5
	defer kpr.certMu.Unlock()
	kpr.cert = &newCert //nolint:wsl_v5
	return nil          //nolint:wsl_v5
}

func (kpr *keypairReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		kpr.certMu.RLock()
		defer kpr.certMu.RUnlock()
		return kpr.cert, nil //nolint:wsl_v5
	}
}
