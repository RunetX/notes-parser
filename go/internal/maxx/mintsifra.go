package maxx

// TLS-доверие для platform-api2.max.ru: цепочка сертификатов MAX завязана
// на корневой и промежуточный CA Минцифры (Russian Trusted Root/Sub CA),
// которых нет в стандартных хранилищах. Официальные PEM кладутся в
// go/internal/maxx/cacert/ (см. README там) и вшиваются в бинарник на
// сборке — self-contained, как tgx.ProxyClient, без зависимости от
// cert-store образа (в distroless/static Russian Trusted CA нет).

import (
	"crypto/tls"
	"crypto/x509"
	"embed"
	"net/http"
	"path"
	"time"
)

//go:embed all:cacert
var cacertFS embed.FS

// MintsifraClient возвращает http.Client с доверием к вшитым CA Минцифры
// поверх системного пула. Если PEM в сборку не положили — nil (транспорт
// SDK по умолчанию, доверие системного хранилища: подходит для хоста, где
// сертификат Минцифры установлен по инструкции Госуслуг).
func MintsifraClient() *http.Client {
	pems := embeddedPEMs()
	if len(pems) == 0 {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, pem := range pems {
		pool.AppendCertsFromPEM(pem)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
			Proxy:           http.ProxyFromEnvironment,
		},
	}
}

// embeddedPEMs — содержимое всех *.pem, вшитых из cacert/.
func embeddedPEMs() [][]byte {
	entries, err := cacertFS.ReadDir("cacert")
	if err != nil {
		return nil
	}
	var pems [][]byte
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".pem" {
			continue
		}
		b, err := cacertFS.ReadFile("cacert/" + e.Name())
		if err != nil {
			continue
		}
		pems = append(pems, b)
	}
	return pems
}
