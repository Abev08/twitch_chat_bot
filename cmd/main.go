package main

import (
	"time"
	"twitch_chat_bot/cmd/chat"
)

func main() {
	chat.Start()

	var sleepDur = time.Second
	for {
		time.Sleep(sleepDur)
	}
}
