package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tgerr"
	"golang.org/x/term"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/go-faster/errors"
	boltstor "github.com/gotd/contrib/bbolt"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/contrib/pebble"
	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/joho/godotenv"
	"github.com/skip2/go-qrcode"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
	lj "gopkg.in/natefinch/lumberjack.v2"
)

var (
	workChat int64
)

func sessionFolder(phone string) string {
	var out []rune
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return "phone-" + string(out)
}

func messageHandler(msg *tg.Message, api *tg.Client, e tg.Entities) error {
	// Проверка, что сообщение из рабочего чата
	if peerID, ok := msg.PeerID.(*tg.PeerChannel); ok && peerID.ChannelID == workChat {
		// Обработка аудиофайлов
		if media, ok := msg.Media.(*tg.MessageMediaDocument); ok {
			if doc, ok := media.Document.(*tg.Document); ok {
				if isAudioFile(doc) {
					fileName := getFileName(doc)
					fmt.Printf("Filename: %s\n", fileName)
					if strings.HasSuffix(strings.ToLower(fileName), ".mp3") {
						// Обработка MP3
						downloadPath := fmt.Sprintf("downloads/%d.mp3", doc.ID)
						if _, err := downloadFile(api, doc, downloadPath); err != nil {
							return errors.Wrap(err, "download mp3")
						}
						fmt.Printf("Download path: %s\n", downloadPath)
						oggPath := fmt.Sprintf("ogg_files/%d.ogg", doc.ID)
						if err := convertMp3ToOgg(downloadPath, oggPath); err != nil {
							return errors.Wrap(err, "convert mp3 to ogg")
						}
						fmt.Printf("Ogg path: %s\n", oggPath)
						if err := sendVoice(api, e, workChat, oggPath); err != nil {
							return errors.Wrap(err, "send voice")
						}
					} else if strings.HasSuffix(strings.ToLower(fileName), ".ogg") {
						// Обработка OGG
						oggPath := fmt.Sprintf("ogg_files/%d.ogg", doc.ID)
						if _, err := downloadFile(api, doc, oggPath); err != nil {
							return errors.Wrap(err, "download ogg")
						}
						if err := sendVoice(api, e, workChat, oggPath); err != nil {
							return errors.Wrap(err, "send voice")
						}
					}
				}
			}
		}

		// Обработка ответов на голосовые сообщения
		if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && msg.Message != "" {
			repliedMsg, err := getMessage(api, e, reply.ReplyToMsgID)
			if err != nil {
				return errors.Wrap(err, "get replied message")
			}
			if repliedMedia, ok := repliedMsg.Media.(*tg.MessageMediaDocument); ok {
				if repliedDoc, ok := repliedMedia.Document.(*tg.Document); ok {
					if isVoiceMessage(repliedDoc) {
						if err := sendVoiceWithCaption(api, e, workChat, repliedDoc, msg.Message, msg.Entities); err != nil {
							return errors.Wrap(err, "send voice with caption")
						}
					}
				}
			}
		}
	}
	return nil
}

func run(ctx context.Context) error {
	var arg struct {
		FillPeerStorage bool
	}
	flag.BoolVar(&arg.FillPeerStorage, "fill-peer-storage", false, "fill peer storage")
	flag.Parse()

	// Загрузка переменных окружения из .env
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "load env")
	}

	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if err != nil {
		return errors.Wrap(err, "parse app id")
	}
	appHash := os.Getenv("APP_HASH")
	if appHash == "" {
		return errors.New("no app hash")
	}
	workChatStr := os.Getenv("WORK_CHAT")
	if workChatStr == "" {
		return errors.New("no organizer chat")
	}
	workChat, err = strconv.ParseInt(workChatStr, 10, 64)
	if err != nil {
		return errors.Wrap(err, "parse organizer chat")
	}

	// Настройка сессии
	sessionDir := filepath.Join("session", sessionFolder("111"))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return err
	}
	logFilePath := filepath.Join(sessionDir, "log.jsonl")
	fmt.Printf("Storing session in %s, logs in %s\n", sessionDir, logFilePath)

	// Настройка логирования
	logWriter := zapcore.AddSync(&lj.Logger{
		Filename:   logFilePath,
		MaxBackups: 3,
		MaxSize:    1,
		MaxAge:     7,
	})
	logCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		logWriter,
		zap.DebugLevel,
	)
	lg := zap.New(logCore)
	defer func() { _ = lg.Sync() }()

	sessionStorage := &telegram.FileSessionStorage{
		Path: filepath.Join(sessionDir, "session.json"),
	}
	db, err := pebbledb.Open(filepath.Join(sessionDir, "peers.pebble.db"), &pebbledb.Options{})
	if err != nil {
		return errors.Wrap(err, "create pebble storage")
	}
	peerDB := pebble.NewPeerStorage(db)
	lg.Info("Storage", zap.String("path", sessionDir))

	// Настройка клиента
	dispatcher := tg.NewUpdateDispatcher()
	updateHandler := storage.UpdateHook(dispatcher, peerDB)
	boltdb, err := bbolt.Open(filepath.Join(sessionDir, "updates.bolt.db"), 0666, nil)
	if err != nil {
		return errors.Wrap(err, "create bolt storage")
	}
	updatesRecovery := updates.New(updates.Config{
		Handler: updateHandler,
		Logger:  lg.Named("updates.recovery"),
		Storage: boltstor.NewStateStorage(boltdb),
	})

	waiter := floodwait.NewWaiter().WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
		lg.Warn("Flood wait", zap.Duration("wait", wait.Duration))
		fmt.Println("Got FLOOD_WAIT. Will retry after", wait.Duration)
	})

	options := telegram.Options{
		Logger:         lg,
		SessionStorage: sessionStorage,
		UpdateHandler:  updatesRecovery,
		Middlewares: []telegram.Middleware{
			waiter,
			ratelimit.New(rate.Every(time.Millisecond*100), 5),
		},
	}
	client := telegram.NewClient(appID, appHash, options)
	api := client.API()

	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}

		err = messageHandler(msg, api, e)
		if err != nil {
			fmt.Println(err)
		}
		return err
	})

	/*dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		fmt.Println("New Message", u.Message)

		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}

		return messageHandler(msg, api)
	})*/

	return waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			authStatus, err := client.Auth().Status(ctx)

			if err != nil {
				return errors.Wrap(err, "get auth status")
			}

			if !authStatus.Authorized {
				_, err := client.QR().Auth(ctx, qrlogin.OnLoginToken(dispatcher), func(ctx context.Context, token qrlogin.Token) error {
					qr, err := qrcode.New(token.URL(), qrcode.Medium)

					if err != nil {
						return err
					}

					code := qr.ToSmallString(false)
					lines := strings.Count(code, "\n")

					fmt.Print(code)
					fmt.Print(strings.Repeat(text.CursorUp.Sprint(), lines))
					return nil
				})

				if err != nil {
					if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
						return fmt.Errorf("qr auth: %w", err)
					}

					fmt.Print("Введите облачный пароль: ")
					password, err := term.ReadPassword(int(os.Stdin.Fd()))
					if err != nil {
						return fmt.Errorf("failed to read password: %w", err)
					}

					passwordStr := strings.TrimSpace(string(password))
					fmt.Println(passwordStr)

					if _, err = client.Auth().Password(ctx, passwordStr); err != nil {
						return fmt.Errorf("password auth: %w", err)
					}
				}
			}

			self, err := client.Self(ctx)
			if err != nil {
				return errors.Wrap(err, "call self")
			}
			name := self.FirstName
			if self.Username != "" {
				name = fmt.Sprintf("%s (@%s)", name, self.Username)
			}
			fmt.Println("Current user:", name)
			lg.Info("Login",
				zap.String("first_name", self.FirstName),
				zap.String("last_name", self.LastName),
				zap.String("username", self.Username),
				zap.Int64("id", self.ID),
			)

			if arg.FillPeerStorage {
				fmt.Println("Filling peer storage from dialogs to cache entities")
				collector := storage.CollectPeers(peerDB)
				if err := collector.Dialogs(ctx, query.GetDialogs(api).Iter()); err != nil {
					return errors.Wrap(err, "collect peers")
				}
				fmt.Println("Filled")
			}

			fmt.Println("Listening for updates. Interrupt (Ctrl+C) to stop.")
			return updatesRecovery.Run(ctx, api, self.ID, updates.AuthOptions{
				IsBot: self.Bot,
				OnStart: func(ctx context.Context) {
					fmt.Println("Update recovery initialized and started, listening for events")
				},
			})
		})
	})
}

// Вспомогательные функции
func isAudioFile(doc *tg.Document) bool {
	for _, attr := range doc.Attributes {
		if _, ok := attr.(*tg.DocumentAttributeAudio); ok {
			return true
		}
	}
	return false
}

func isVoiceMessage(doc *tg.Document) bool {
	for _, attr := range doc.Attributes {
		audioAttr, ok := attr.(*tg.DocumentAttributeAudio)
		fmt.Println(audioAttr, ok)
	}

	for _, attr := range doc.Attributes {
		if audioAttr, ok := attr.(*tg.DocumentAttributeAudio); ok && audioAttr.Voice {
			return true
		}
	}
	return false
}

func getFileName(doc *tg.Document) string {
	for _, attr := range doc.Attributes {
		if fnAttr, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return fnAttr.FileName
		}
	}
	return fmt.Sprintf("%d", doc.ID)
}

func downloadFile(api *tg.Client, doc *tg.Document, path string) (tg.StorageFileTypeClass, error) {
	// Создаём директорию для скачиваний, если она не существует
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("failed to create download directory: %w", err)
	}

	location := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}
	d := downloader.NewDownloader()
	return d.Download(api, location).ToPath(context.Background(), path)
}

func convertMp3ToOgg(inputPath, outputPath string) error {
	// Проверяем, существует ли файл outputPath
	if _, err := os.Stat(outputPath); err == nil {
		// Файл существует, возвращаем nil
		return nil
	} else if !os.IsNotExist(err) {
		// Если ошибка не связана с отсутствием файла, возвращаем её
		return fmt.Errorf("failed to check output file: %w", err)
	}

	// Создаём директорию для outputPath, если она не существует
	if err := os.MkdirAll(filepath.Dir(outputPath), 0700); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Выполняем конвертацию с помощью ffmpeg
	cmd := exec.Command("ffmpeg", "-i", inputPath, "-c:a", "libopus", outputPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert mp3 to ogg: %w", err)
	}

	return nil
}

func sendVoice(api *tg.Client, e tg.Entities, chatID int64, oggPath string) error {
	var accessHash int64
	for _, channel := range e.Channels {
		if channel.ID == workChat {
			accessHash = channel.AccessHash
		}
	}

	file, err := os.Open(oggPath)
	if err != nil {
		return err
	}
	defer func(file *os.File) {
		_ = file.Close()
	}(file)

	u := uploader.NewUploader(api)
	uploadedFile, err := u.FromFile(context.Background(), file)
	if err != nil {
		return err
	}
	attributes := []tg.DocumentAttributeClass{
		&tg.DocumentAttributeAudio{Voice: true},
	}
	media := tg.InputMediaUploadedDocument{
		File:       uploadedFile,
		Attributes: attributes,
		MimeType:   "audio/ogg",
	}
	_, err = api.MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: chatID, AccessHash: accessHash},
		Media:    &media,
		RandomID: rand.Int63(),
	})
	return err
}

func getMessage(api *tg.Client, e tg.Entities, msgID int) (*tg.Message, error) {
	var accessHash int64
	for _, channel := range e.Channels {
		if channel.ID == workChat {
			accessHash = channel.AccessHash
		}
	}

	resp, err := api.ChannelsGetMessages(
		context.Background(),
		&tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: workChat, AccessHash: accessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
		},
	)
	if err != nil {
		return nil, err
	}
	messages := resp.(*tg.MessagesChannelMessages).GetMessages()
	for _, m := range messages {
		if msg, ok := m.(*tg.Message); ok {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message not found")
}

func sendVoiceWithCaption(api *tg.Client, e tg.Entities, chatID int64, doc *tg.Document, caption string, entities []tg.MessageEntityClass) error {
	var accessHash int64
	for _, channel := range e.Channels {
		if channel.ID == workChat {
			accessHash = channel.AccessHash
		}
	}

	media := &tg.InputMediaDocument{
		ID: &tg.InputDocument{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		},
	}
	_, err := api.MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: chatID, AccessHash: accessHash},
		Media:    media,
		Message:  caption,
		Entities: entities,
		RandomID: rand.Int63(),
	})
	return err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			fmt.Println("\rClosed")
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		os.Exit(1)
	} else {
		fmt.Println("Done")
		os.Exit(0)
	}
}
