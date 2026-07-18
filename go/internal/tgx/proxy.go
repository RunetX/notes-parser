package tgx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// httpClientTimeout — верхняя граница на один запрос к Bot API. Больше
// таймаута long polling (pollTimeout), чтобы не рвать ожидание обновлений.
const httpClientTimeout = 60 * time.Second

// ProxyClient строит HTTP-клиент для Bot API через прокси. Поддерживаются
// схемы http/https и socks5/socks5h (можно с логином:паролем в URL).
// Пустая строка — прямое соединение (возвращается nil, клиент по умолчанию).
// Полезно, когда сайт доступен только с российского IP, а Telegram из
// России — только через прокси.
func ProxyClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("разбор telegram_proxy %q: %w", proxyURL, err)
	}
	transport := &http.Transport{}
	switch u.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5-прокси: %w", err)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = cd.DialContext
		} else {
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		return nil, fmt.Errorf("неизвестная схема прокси %q (нужно http/https/socks5)", u.Scheme)
	}
	return &http.Client{Transport: transport, Timeout: httpClientTimeout}, nil
}
