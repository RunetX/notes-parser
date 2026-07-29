package love

// Доменные типы личной переписки сайта (talks). Здесь только структуры данных;
// клиентские методы (TalksDialogs/TalksHistory/TalksSend поверх JSON-эндпоинтов
// сайта) добавляются в Ф4 после разведки API — см. briefs/love-talks-telegram.md.

import "time"

// TalkDialog — один диалог в списке talks (метаданные для поллера и списка).
type TalkDialog struct {
	PassportID   string    // адресат диалога (/talks/<passport_id>)
	ProfileID    string    // id анкеты /profile/<id>/, если сайт его отдаёт
	Nick         string    // ник собеседника
	AvatarURL    string    // аватар собеседника (опц.)
	LastMsgID    string    // id последнего сообщения — сравниваем с курсором
	Unread       int       // непрочитанных (справочно; сигнал — курсор)
	LastActivity time.Time // время последней активности; zero — неизвестно
}

// TalkMessage — одно сообщение диалога talks.
type TalkMessage struct {
	SiteMsgID string    // id сообщения на сайте
	FromSelf  bool      // true — исходящее (написано владельцем сессии)
	Text      string    // текст сообщения
	MediaURL  string    // вложение (фото), если есть
	SentAt    time.Time // время по сайту; zero — неизвестно
}
