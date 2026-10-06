// Command compare serves the comparison host: the native /k8s proxy, four gateway stream routes,
// the save endpoints, the page and its counters, against the kubeconfig's cluster, in a scratch
// namespace it deletes on exit.
//
//	task compare            # serve the page at http://127.0.0.1:8110/
//	task compare-measure    # run the measurement matrix against it
//
// It listens twice. --tls-addr serves HTTPS with HTTP/2 under a self-signed certificate made at
// startup: that is the address for a browser. A page holding six streams over HTTP/1.1 would have
// used every connection a browser allows one origin, and its saves would wait behind them. --addr
// serves plain HTTP/1.1 for drivers such as measure.ts and curl.
//
// It prints one line, `ready <url> <tls url> namespace=<ns>`, once it is serving.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ConfigButler/krm-stream/gateway/kube/examples/comparison"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	defaultKubeconfig := os.Getenv("KUBECONFIG")
	if defaultKubeconfig == "" {
		defaultKubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig, "path to the kubeconfig whose current context is the cluster")
	addr := flag.String("addr", "127.0.0.1:8110", "plain HTTP/1.1 listen address for drivers; keep it on loopback, the admin routes have no other protection")
	tlsAddr := flag.String("tls-addr", "127.0.0.1:8111", "HTTPS (HTTP/2, self-signed) listen address for browsers; empty disables it")
	dist := flag.String("dist", "../../packages/krm-stream/dist", "built library directory served at /packages/krm-stream/dist/")
	page := flag.String("page", "../../examples/comparison", "the comparison page directory served at /examples/comparison/")
	widgets := flag.Int("widgets", 8, "number of Widgets")
	secrets := flag.Int("secrets", 3, "number of Secrets")
	viewer := flag.String("viewer-session", "viewer-session", "session token every source accepts")
	refused := flag.String("refused-session", "refused-session", "session token that is authenticated and refused by every source")
	reauthorize := flag.Duration("reauthorize-every", 5*time.Second, "gateway timed authorization recheck interval")
	prefix := flag.String("namespace-prefix", "krm-compare", "scratch namespace prefix")
	flag.Parse()

	if err := run(*kubeconfig, *addr, *tlsAddr, comparison.Config{
		NamespacePrefix: *prefix, Widgets: *widgets, Secrets: *secrets,
		ViewerToken: *viewer, RefusedToken: *refused, ReauthorizeEvery: *reauthorize,
		Dist: *dist, Page: *page,
	}); err != nil {
		log.Fatal(err)
	}
}

func run(kubeconfig, addr, tlsAddr string, cfg comparison.Config) error {
	cluster, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.Cluster = cluster
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	setup, cancel := context.WithTimeout(ctx, 2*time.Minute)
	host, err := comparison.New(setup, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := host.Close(cleanup); err != nil {
			log.Printf("cleanup: %v", err)
			return
		}
		log.Printf("deleted namespace %s", host.Namespace())
	}()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	plain := &http.Server{Handler: host, ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{plain}
	serve := []func() error{func() error { return plain.Serve(listener) }}
	urls := "http://" + listener.Addr().String() + "/"
	if tlsAddr != "" {
		cert, err := selfSigned()
		if err != nil {
			return err
		}
		tlsListener, err := net.Listen("tcp", tlsAddr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		secure := &http.Server{
			Handler:           host,
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		}
		servers = append(servers, secure)
		// ServeTLS negotiates HTTP/2.
		serve = append(serve, func() error { return secure.ServeTLS(tlsListener, "", "") })
		urls += " https://" + tlsListener.Addr().String() + "/"
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdown)
		}
	}()
	fmt.Printf("ready %s namespace=%s\n", urls, host.Namespace())
	errs := make(chan error, len(serve))
	for _, s := range serve {
		go func() { errs <- s() }()
	}
	var first error
	for range serve {
		if err := <-errs; err != nil && !errors.Is(err, http.ErrServerClosed) && first == nil {
			first = fmt.Errorf("serve: %w", err)
			stop()
		}
	}
	return first
}

// selfSigned makes a short-lived certificate for loopback, so a browser can speak HTTP/2 to this
// host. The browser warns once; accept it for 127.0.0.1 only.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "krm-stream comparison (loopback)"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
