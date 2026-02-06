// Shared HTTP transport creation for backends.
// Eliminates duplication between OpenAI and LangGraph Init() methods.

package backends

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// TransportConfig holds settings for creating an HTTP transport.
type TransportConfig struct {
	BaseURL     string // API base URL (determines protocol: http:// or https://)
	SocketPath  string // Unix socket path for HTTP/1.1 mode (empty = TCP)
	UseHTTP2    bool   // Enable HTTP/2 multiplexing
	InsecureSSL bool   // Skip TLS certificate verification (testing only)
	Label       string // For log messages (e.g., "OpenAI", "LangGraph")
}

// NewHTTPClient creates an *http.Client with the appropriate transport
// based on the configuration. Selects one of four transport modes:
//
//   - HTTP/2 h2c:  UseHTTP2=true  + http:// base URL
//   - HTTP/2 ALPN: UseHTTP2=true  + https:// base URL
//   - HTTP/1.1 Unix socket: UseHTTP2=false + SocketPath set
//   - HTTP/1.1 TCP: fallback
func NewHTTPClient(cfg TransportConfig) *http.Client {
	var roundTripper http.RoundTripper

	if cfg.UseHTTP2 && strings.HasPrefix(cfg.BaseURL, "http://") {
		// HTTP/2 with h2c (prior knowledge) for http:// endpoints
		roundTripper = &http2.Transport{
			AllowHTTP:          true,
			DisableCompression: true,
			DialTLSContext: func(ctx context.Context, network, addr string, tlsCfg *tls.Config) (net.Conn, error) {
				d := net.Dialer{KeepAlive: 30 * time.Second}
				return d.DialContext(ctx, network, addr)
			},
		}
		slog.Info("backend transport configured", "backend", cfg.Label, "url", cfg.BaseURL, "mode", "HTTP/2 h2c")
	} else if cfg.UseHTTP2 && strings.HasPrefix(cfg.BaseURL, "https://") {
		// HTTP/2 with ALPN negotiation for https:// endpoints
		tlsConfig := &tls.Config{}
		if cfg.InsecureSSL {
			tlsConfig.InsecureSkipVerify = true
		}
		roundTripper = &http.Transport{
			TLSClientConfig:     tlsConfig,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		slog.Info("backend transport configured", "backend", cfg.Label, "url", cfg.BaseURL, "mode", "HTTP/2 ALPN")
	} else if cfg.SocketPath != "" {
		// HTTP/1.1 with Unix socket for high-concurrency
		roundTripper = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext(ctx, "unix", cfg.SocketPath)
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
		slog.Info("backend transport configured", "backend", cfg.Label, "url", cfg.BaseURL, "mode", "HTTP/1.1 unix", "socket", cfg.SocketPath)
	} else {
		// HTTP/1.1 with TCP
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
		if strings.HasPrefix(cfg.BaseURL, "https://") && cfg.InsecureSSL {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		roundTripper = transport
		slog.Info("backend transport configured", "backend", cfg.Label, "url", cfg.BaseURL, "mode", "HTTP/1.1")
	}

	return &http.Client{
		Transport: roundTripper,
	}
}
