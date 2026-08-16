package main

// Мелочи, общие для команд CLI.

import (
	"log/slog"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
)

// newSiteClient — клиент сайта по конфигу. Адрес, User-Agent и интервал
// запросов у всех команд одни: разъехавшийся интервал у одной из них — это
// лишний повод для DDoS-Guard, а не мелочь. log == nil — молча (doctor).
func newSiteClient(cfg *config.Config, log *slog.Logger) *love.Client {
	return love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
}
