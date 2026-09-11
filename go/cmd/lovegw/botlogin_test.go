package main

// Склейка двух миров у привязки мессенджера проверяется ЗДЕСЬ, потому что это
// единственный пакет, который видит оба.

import (
	"testing"

	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// ИМЯ МЕССЕНДЖЕРА ОДНО НА ДВА МИРА, и стеречь это обязан тест, а не внимание.
//
// Бот знает себя как store.Messenger*, площадка хранит привязку в
// identities.kind и сверяет вид по закрытому списку (platform.Identity*). Между
// ними НЕТ перевода — строка едет через адаптер как есть, — и разойтись они
// могут только молча: привязка стала бы отвечать «неизвестный мессенджер», а
// человек увидел бы внутреннюю ошибку на ровном месте.
//
// Тот же приём, что у narod.TempoWindow и archive.TempoWindow: равенство двух
// констант проверяется в пакете, которому видны обе.
func TestMessengerNamesMatchAcrossWorlds(t *testing.T) {
	for _, c := range []struct{ bot, plat string }{
		{store.MessengerTelegram, platform.IdentityTelegram},
		{store.MessengerMax, platform.IdentityMAX},
	} {
		if c.bot != c.plat {
			t.Errorf("бот зовёт мессенджер %q, площадка — %q", c.bot, c.plat)
		}
	}
}
