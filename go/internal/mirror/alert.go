package mirror

// Доменные ключи и порог уведомлений админу; сам троттлер — internal/alerts.

const alertThreshold = 3 // подряд неудач, прежде чем беспокоить админа

const (
	keyFeedDrift     = "дрейф вёрстки ленты"
	keyCommentsDrift = "дрейф вёрстки комментариев"
	keyForbidden     = "доступ к сайту (403)"
)
