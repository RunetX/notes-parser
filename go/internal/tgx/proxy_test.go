package tgx

import "testing"

func TestProxyClientEmptyIsDirect(t *testing.T) {
	c, err := ProxyClient("")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Errorf("пустой прокси должен давать nil (прямое соединение), получено %v", c)
	}
}

func TestProxyClientSchemes(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:8080",
		"https://proxy.example:3128",
		"socks5://127.0.0.1:1080",
		"socks5://user:pass@127.0.0.1:1080",
	} {
		c, err := ProxyClient(url)
		if err != nil {
			t.Errorf("ProxyClient(%q): %v", url, err)
			continue
		}
		if c == nil || c.Transport == nil {
			t.Errorf("ProxyClient(%q): пустой клиент", url)
		}
	}
}

func TestProxyClientBadScheme(t *testing.T) {
	if _, err := ProxyClient("ftp://x:1"); err == nil {
		t.Error("ожидалась ошибка на неизвестной схеме прокси")
	}
}
