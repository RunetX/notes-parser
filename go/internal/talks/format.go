package talks

// Компоновка входящего ЛС в HTML-сообщение мессенджера. Разметка (<b>/<a>)
// одинаково понятна и Telegram, и MAX (сервер MAX разбирает format=html в
// text+markup — см. бриф гейта MAX). Текст берётся «живым» из истории сайта,
// а не из БД, поэтому доставка работает и при store_text=false.

import (
	"html"
	"strings"

	"lovegw/internal/love"
	"lovegw/internal/store"
	"lovegw/internal/textutil"
)

// maxTextLen — предел длины текста в одном сообщении (запас под лимиты
// мессенджеров и разметку).
const maxTextLen = 3500

// formatIncoming строит HTML входящего сообщения: «💌 <b>Ник</b> · анкета» и текст.
func formatIncoming(baseURL string, peer store.TalkPeer, m love.TalkMessage) string {
	var b strings.Builder
	b.WriteString("💌 <b>")
	b.WriteString(html.EscapeString(nickOr(peer.Nick)))
	b.WriteString("</b>")
	if link := profileLink(baseURL, peer.ProfileID); link != "" {
		b.WriteString(" · ")
		b.WriteString(link)
	}
	b.WriteString("\n\n")
	if t := strings.TrimSpace(m.Text); t != "" {
		b.WriteString(html.EscapeString(textutil.Truncate(t, maxTextLen)))
	}
	if m.MediaURL != "" {
		if strings.TrimSpace(m.Text) != "" {
			b.WriteString("\n")
		}
		b.WriteString("📷 <a href=\"")
		b.WriteString(html.EscapeString(m.MediaURL))
		b.WriteString("\">фото</a>")
	}
	return b.String()
}

func nickOr(nick string) string {
	if strings.TrimSpace(nick) == "" {
		return "Собеседник"
	}
	return nick
}

// profileLink — ссылка на анкету собеседника; "" если id или baseURL нет.
func profileLink(baseURL, profileID string) string {
	if profileID == "" || baseURL == "" {
		return ""
	}
	return "<a href=\"" + html.EscapeString(strings.TrimRight(baseURL, "/")+"/profile/"+profileID+"/") + "\">анкета</a>"
}
