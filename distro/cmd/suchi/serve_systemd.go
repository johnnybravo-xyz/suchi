package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"
)

type serveOptions struct {
	systemd        bool
	setupTokenFile string
}

func parseServeOptions(args []string) (serveOptions, error) {
	var options serveOptions
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.BoolVar(&options.systemd, "systemd", false, "notify systemd after the HTTP listener is ready (Linux only)")
	flags.StringVar(&options.setupTokenFile, "setup-token-file", "", "write the first-run setup token to a private file instead of logs")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return options, nil
}

func serveHTTP(server *http.Server, certFile, keyFile string, notify *net.UnixConn) error {
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		if server.TLSConfig == nil {
			server.TLSConfig = &tls.Config{}
		} else {
			server.TLSConfig = server.TLSConfig.Clone()
		}
		server.TLSConfig.Certificates = []tls.Certificate{certificate}
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("bind HTTP listener: %w", err)
	}
	defer listener.Close()
	if err := notifySystemdReady(notify); err != nil {
		return err
	}
	if certFile != "" {
		// ServeTLS retains Go's HTTP/2 setup; the certificate is loaded before READY.
		return server.ServeTLS(listener, "", "")
	}
	return server.Serve(listener)
}

func notifySystemdReady(notify *net.UnixConn) error {
	if notify == nil {
		return nil
	}
	if err := notify.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set systemd notification deadline: %w", err)
	}
	if _, err := notify.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("notify systemd readiness: %w", err)
	}
	return nil
}
