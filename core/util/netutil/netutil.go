package netutil

import (
	"net/url"

	"github.com/go-faster/errors"
	"github.com/iyear/connectproxy"
	"golang.org/x/net/proxy"
)

// NewProxy builds a dialer for proxyUrl, verifying the TLS certificate that an
// https:// proxy presents. Use NewProxyInsecure when the proxy terminates TLS
// with a certificate this host does not trust.
func NewProxy(proxyUrl string) (proxy.ContextDialer, error) {
	return newProxy(proxyUrl, false)
}

// NewProxyInsecure is NewProxy without verification of the proxy's own TLS
// certificate. Only the connection to the proxy is affected: traffic tunnelled
// through it keeps its own end-to-end TLS.
func NewProxyInsecure(proxyUrl string) (proxy.ContextDialer, error) {
	return newProxy(proxyUrl, true)
}

func newProxy(proxyUrl string, insecureSkipVerify bool) (proxy.ContextDialer, error) {
	u, err := url.Parse(proxyUrl)
	if err != nil {
		return nil, errors.Wrap(err, "parse proxy url")
	}

	// HTTP(S) proxies tunnel with the CONNECT verb and are built here instead
	// of through proxy.FromURL, so the verification setting stays a property of
	// this call rather than process-global state.
	if u.Scheme == "http" || u.Scheme == "https" {
		d, err := connectproxy.NewWithConfig(u, proxy.Direct, &connectproxy.Config{
			InsecureSkipVerify: insecureSkipVerify,
		})
		if err != nil {
			return nil, errors.Wrap(err, "connect proxy")
		}
		return d, nil
	}

	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, errors.Wrap(err, "proxy from url")
	}

	if d, ok := dialer.(proxy.ContextDialer); ok {
		return d, nil
	}

	return nil, errors.New("proxy dialer is not ContextDialer")
}
