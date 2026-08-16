package tgx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	transport, err := ProxyTransport(proxyURL)
	if err != nil || transport == nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: httpClientTimeout}, nil
}

// MaskProxy прячет пароль в строке прокси, оставляя всё, что нужно для
// диагностики: схему, логин, хост и порт. Строку печатает doctor и она попадает
// в тексты ошибок, а их читают через плечо, копируют в переписку и прикладывают
// к отчётам — пароль в таком месте живёт дольше, чем нужно.
func MaskProxy(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		// Неразбираемую строку не показываем вовсе: пароль в ней всё равно
		// есть, а диагностике довольно самого факта, что она не разбирается.
		return "<строка прокси не разбирается>"
	}
	name := ""
	if u.User != nil {
		if _, ok := u.User.Password(); !ok {
			return proxyURL // логин без пароля прятать нечего
		}
		name = u.User.Username()
	}
	if name == "" {
		return proxyURL
	}
	u.User = nil
	// Собираем руками: url.UserPassword экранировала бы звёздочки в %2A.
	return u.Scheme + "://" + name + ":***@" + strings.TrimPrefix(u.String(), u.Scheme+"://")
}

// ProxyTransport строит HTTP-транспорт через прокси — для клиентов с другим
// таймаутом, чем у Bot API (например, минуты у LLM-запросов дайджеста).
// Пустая строка — прямое соединение (nil, nil).
func ProxyTransport(proxyURL string) (*http.Transport, error) {
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// Строку целиком в ошибку не кладём: в ней пароль, а ошибка уедет и в
		// лог, и в вывод doctor. Своей обёртки мало — *url.Error несёт разобранную
		// строку внутри себя, поэтому берём только причину.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("разбор telegram_proxy (%s): %w", MaskProxy(proxyURL), err)
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
	return transport, nil
}
