package chat

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Temporary variables, should be in some sort of config package

var PASS string = "" // OAuth token
var NICK string = "AbevBot" // Chat bot nick
var CHANNEL_NAME string = "AbevBot" // Channel name




var PrintChatMessages = true // Should chat messages be printed to stdout?

const messageSendCooldown = time.Millisecond * 200 // Minimum 200ms between messages sent
var messageStart = []byte("@badge") // Byte array describing chat message start
var messageEnd = []byte("\r\n") // Byte array describing chat message end
var sendQueue messageQueue // Queue of chat messages to send to chat
var isStarted bool // Is the chat bot started?
var chatMessagesSinceLastPeriodicMessage uint16 // Amount of chat messages since last periodic message

// Starts the chat bot.
func Start() {
	if isStarted {
		return
	}
	isStarted = true

	slog.Info("Chat bot starting")
	go update()
}

// Main update.
func update() {
	var sleepErrorDur = time.Second * 5
	var receiveBuffer []byte = make([]byte, 16384) // Max IRC message is 4096 bytes? let's allocate 4 times that, 2 times max message length wasn't enaugh for really fast chats
	var remainingData []byte = make([]byte, 16384)
	var remainingDataLen int
	var zeroBytesReceivedCounter uint8
	var header, body, msg string
	var messageMetadata messageMetadata
	var lastMessageSent time.Time = time.Now()

	for {
		// Try to connect
		slog.Info("Chat bot connecting...")
		var conn, err = net.Dial("tcp", "irc.chat.twitch.tv:6667")
		if err != nil {
			slog.Error("Chat bot error.", "Err", err)
			time.Sleep(sleepErrorDur)
			continue
		}

		// Connected! Send authentication data
		slog.Info("Chat bot connected!")
		{
			var builder strings.Builder
			builder.WriteString(fmt.Sprintf("PASS oauth:%s\r\n", PASS))
			builder.WriteString(fmt.Sprintf("NICK %s\r\n", NICK))
			builder.WriteString(fmt.Sprintf("JOIN #%s,#%s\r\n", strings.ToLower(CHANNEL_NAME), strings.ToLower(CHANNEL_NAME)))
			builder.WriteString("CAP REQ :twitch.tv/commands twitch.tv/tags\r\n")
			_, err = conn.Write([]byte(builder.String()))
			if err != nil {
				slog.Error("Chat bot error.", "Err", err)
				continue
			}
			zeroBytesReceivedCounter = 0
		}

		// Update loop
		for {
			// Receive messages
			conn.SetReadDeadline(time.Now().Add(time.Millisecond * 10)) // This timeout slows down entire loop, so no additional sleep is necessary
			var n, err = conn.Read(receiveBuffer)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					// Read deadline exceeded, nothing to do
				} else {
					slog.Error("Chat bot error.", "Err", err)
					break
				}
			} else if n == 0 {
				zeroBytesReceivedCounter++
				if zeroBytesReceivedCounter > 5 {
					slog.Warn("Chat bot received 0 bytes multiple times, reconnecting!")
					break
				}
			} else {
				// Check if received data starts with message start
				var newMessage = true
				for i, v := range messageStart {
					if receiveBuffer[i] != v {
						newMessage = false
						break
					}
				}

				// Look through received data and look for message end
				var start, end int
				for i := 0; i < n; i++ {
					var endFound = true
					for j, v := range messageEnd {
						if receiveBuffer[i+j] != v {
							endFound = false
							break
						}
					}
					if endFound {
						i += len(messageEnd) // Add message end len
						end = i
						if end-start <= 2 {
							// Just an empty "\r\n", skip
						} else if newMessage {
							// Is there left over data? Just try to parse it?
							if remainingDataLen > 0 {
								header, body, msg, messageMetadata = parseMessage(remainingData[:remainingDataLen])
								processMessage(header, body, msg, messageMetadata)
								remainingDataLen = 0
							}

							// Parse new message
							header, body, msg, messageMetadata = parseMessage(receiveBuffer[start:end])
							processMessage(header, body, msg, messageMetadata)
						} else {
							// Append start of a message to left over data and parse it
							for k := 0; k < end; k++ {
								remainingData[remainingDataLen+k] = receiveBuffer[k]
							}
							header, body, msg, messageMetadata = parseMessage(remainingData[:(remainingDataLen + end)])
							processMessage(header, body, msg, messageMetadata)
							remainingDataLen = 0
						}

						newMessage = true // After parsing at least 1 message, parse the rest like new messages
						start = end
					}
				}

				if end < n {
					// Data is missing message end
					remainingDataLen = n - end
					for i := 0; i < remainingDataLen; i++ {
						remainingData[i] = receiveBuffer[end+i]
					}
				}
			}

			// Send messages
			if sendQueue.PendingMessages && time.Since(lastMessageSent) > messageSendCooldown {
				var msg, err = sendQueue.pop()
				if err != nil {
					slog.Error("Chat bot error, when sending a message.", "Err", err)
				} else {
					conn.Write([]byte(msg))
					lastMessageSent = time.Now()
				}
			}

			// Periodic messages
		}

		conn.Close()
		time.Sleep(sleepErrorDur)
	}
}

// Parses chat message, returning header, body, whole message and metadata parts.
func parseMessage(data []byte) (header, body, msg string, metadata messageMetadata) {
	msg = string(data)
	var temp, temp2, temp3 int

	if strings.HasPrefix(msg, "PING") {
		header = msg
		return
	}

	// Find header <-> body "separator"
	temp = strings.Index(msg, "tmi.twitch.tv")
	if temp < 0 {
		slog.Warn("Chat message not parsed correctly.\n", "Msg", msg)
		return
	}

	// Get message header
	header = msg[:temp]
	temp += 14 // 14 == "tmi.twitch.tv ".len()

	// Get message type
	temp2 = strings.Index(msg[temp:], " ")
	if temp2 < 0 {
		temp2 = len(msg) - temp
	}
	metadata.MessageType = msg[temp:(temp + temp2)]
	temp2 += 1 + temp

	// Get message body
	temp = strings.Index(msg[temp2:], ":")
	if temp > 0 {
		body = msg[(temp + 1 + temp2):]
	}
	body = strings.TrimSuffix(body, "\r\n")
	body = strings.TrimSuffix(body, "\r")
	body = strings.TrimSuffix(body, "\n")

	// Get header data
	temp = 0  // start
	temp2 = 0 // end
	temp3 = 0 // '=' index
	for i, v := range header {
		if v == ';' || v == ' ' {
			temp2 = i
		} else if v == '=' {
			temp3 = i + 1
		}

		if temp2 > temp {
			var s = header[temp3:temp2]
			switch header[temp:(temp3 - 1)] {
			case "id":
				metadata.MessageID = s
			case "badges":
				if strings.HasPrefix(s, "broadcaster") {
					metadata.Badge = "STR"
				} else if strings.HasPrefix(s, "moderator") {
					metadata.Badge = "MOD"
				} else if strings.HasPrefix(s, "subscriber") {
					metadata.Badge = "SUB"
				} else if strings.HasPrefix(s, "vip") {
					metadata.Badge = "VIP"
				}
			case "display-name":
				metadata.UserName = s
			case "user-id":
				var num, err = strconv.ParseInt(s, 10, 64)
				if err == nil {
					metadata.UserID = num
				}
			case "custom-reward-id":
				metadata.CustomRewardID = s
			case "bits":
				metadata.Bits = s
			case "@msg-id":
				fallthrough
			case "msg-id":
				metadata.MsgID = s
			case "msg-param-recipient-display-name":
				metadata.Receipent = s
			}
			temp = temp2 + 1
			temp3 = temp
		}
	}
	return
}

// Processes the parsed chat message.
func processMessage(header, body, msg string, metadata messageMetadata) {
	if strings.HasPrefix(header, "PING") {
		sendQueue.push("PONG :tmi.twitch.tv\r\n")
		return
	}

	switch metadata.MessageType {
	case "PRIVMSG":
		if len(metadata.CustomRewardID) > 0 {
			slog.Info("Chatter redeemed custom reward!",
				"ChatterName", metadata.UserName,
				"RewardID", metadata.CustomRewardID,
				"Message", body)
		} else if len(metadata.Bits) > 0 {
			slog.Info("Chatter cheered with bits!",
				"ChatterName", metadata.UserName,
				"Bits", metadata.Bits,
				"Message", body)
		} else {
			chatMessagesSinceLastPeriodicMessage++
			if PrintChatMessages {
				fmt.Printf("%3s %20s: %s\n", metadata.Badge, metadata.UserName, body)
			}
			checkForChatCommands(body, metadata)
		}

	case "USERNOTICE":
		switch metadata.MsgID {
		case "sub":
			slog.Info("Subscription", "ChatterName", metadata.UserName, "Message", body)
		case "resub":
			slog.Info("Resubscription", "ChatterName", metadata.UserName, "Message", body)
		case "subgift":
			slog.Info("Subscription gift", "ChatterName", metadata.UserName, "Receipent", metadata.Receipent, "Message", body)
		case "submysterygift":
			slog.Info("Subscription gift to random chatters", "ChatterName", metadata.UserName, "Message", body)
		case "primepaidupgrade":
			slog.Info("Subscription prime upgrade", "ChatterName", metadata.UserName, "Message", body)
		case "giftpaidupgrade":
			slog.Info("Subscription gift upgrade", "ChatterName", metadata.UserName, "Message", body)
		case "communitypayforward":
			slog.Info("Subscription gifted is payed forward", "ChatterName", metadata.UserName, "Message", body)
		case "announcement":
			slog.Info("Announcement", "ChatterName", metadata.UserName, "Message", body)
		case "raid":
			slog.Info("Raid", "ChatterName", metadata.UserName, "Message", body)
		case "viewermilestone":
			slog.Info("Chatter reached viewer milestone", "ChatterName", metadata.UserName, "Message", body)
		default:
			// Message type not recognized - print the whole message
			fmt.Print(msg)
		}

	case "CLEARCHAT":
		if strings.HasPrefix(header, "@ban-duration") {
			slog.Info("Chatter got banned", "ChatterName", body)
		} else if len(body) > 0 {
			slog.Info("Chat messages got cleared", "ChatterName", body)
		} else {
			slog.Info("Chat got cleared")
		}

	case "CLEARMSG":
		if strings.HasPrefix(header, "@login=") {
			var idx = strings.Index(header, ";")
			if idx < 0 {
				idx = len(header)
			}
			slog.Info("Chatter got perma banned", "ChatterName", header[7:idx])
		} else {
			slog.Info("Someones messages got cleared")
		}

	case "NOTICE":
		switch metadata.MsgID {
		case "emote_only_on":
			slog.Info("This room is now in emote-only mode.")
		case "emote_only_off":
			slog.Info("This room is no longer in emote-only mode.")
		case "subs_on":
			slog.Info("This room is now in subscribers-only mode.")
		case "subs_off":
			slog.Info("This room is no longer in subscribers-only mode.")
		case "followers_on":
			fallthrough
		case "followers_on_zero":
			slog.Info("This room is now in followers-only mode.")
		case "followers_off":
			slog.Info("This room is no longer in followers-only mode.")
		case "msg_followersonly":
			slog.Info("This room is in 10 minutes followers-only mode.")
		case "slow_on":
			slog.Info("This room is now in slow mode.")
		case "slow_off":
			slog.Info("This room is no longer in slow mode.")
		default:
			// Message type not recognized - print the whole message
			fmt.Print(msg)
		}

	case "ROOMSTATE":
		// Room state changed - do nothing? This message is always send with another one?

	case "USERSTATE":
		if PrintChatMessages {
			fmt.Printf("BOT %20s: %s\n", metadata.UserName, body)
		}

	default:
		// Not recognized message
		fmt.Print(msg)
	}
}

// Checks chat message for commands.
func checkForChatCommands(msg string, metadata messageMetadata) {
	// if strings.HasPrefix(msg, "!time") {
	// 	SendMessageResponse(time.Now().String(), metadata.MessageID)
	// }
}

// Sends text message to chat.
func SendMessage(msg string) {
	SendMessageResponse(msg, "")
}

// Sends text message response to chat.
func SendMessageResponse(msg, msgID string) {
	if !isStarted || len(msg) == 0 {
		return
	}

	var sb strings.Builder
	// int start = 0;
	// int end = message.Length > MESSAGESENTMAXLEN ? MESSAGESENTMAXLEN : message.Length;

	// while (true)
	// {
	//   sb.Clear();
	if len(msgID) > 0 {
		sb.WriteString("@reply-parent-msg-id=")
		sb.WriteString(msgID)
		sb.WriteString(" ")
	}
	sb.WriteString("PRIVMSG #")
	sb.WriteString(strings.ToLower(CHANNEL_NAME))
	sb.WriteString(" :")
	sb.WriteString(msg)
	sb.WriteString("\r\n")

	sendQueue.push(sb.String())

	// if (end == message.Length) break;
	// start = end + 1;
	// end += MESSAGESENTMAXLEN;
	// if (end > message.Length) end = message.Length;
	// }
}

// Chat message metadata
type messageMetadata struct {
	UserID         int64  // Chatter ID
	UserName       string // Name of the chatter
	Badge          string // Badge of the chatter
	MessageType    string // Type of the chat message
	MessageID      string // Message ID
	CustomRewardID string // Custom reward ID that created the chat message
	Bits           string // Amount of bits
	MsgID          string // Type of special chat message (like "sub", "emote_only_on")
	Receipent      string // Receipent of action from a chat message (like receipent of sub gift)
}

// Queue of chat messages to send to chat.
type messageQueue struct {
	PendingMessages bool
	mutex           sync.Mutex
	Queue           []string
}

// Add text message to send queue.
func (q *messageQueue) push(msg string) {
	q.mutex.Lock()
	q.Queue = append(sendQueue.Queue, msg)
	q.PendingMessages = true
	q.mutex.Unlock()
}

// Take first message from the queue.
func (q *messageQueue) pop() (msg string, err error) {
	q.mutex.Lock()
	var count = len(q.Queue)
	if count == 0 {
		err = errors.New("queue is empty")
		return
	}
	msg = q.Queue[0]
	q.Queue = q.Queue[1:]
	q.PendingMessages = count > 1
	q.mutex.Unlock()
	return
}
