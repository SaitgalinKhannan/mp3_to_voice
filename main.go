package main

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/joho/godotenv"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
	lj "gopkg.in/natefinch/lumberjack.v2"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

var (
	workChat int64 // ID рабочего чата
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

	phone := os.Getenv("TG_PHONE")
	if phone == "" {
		return errors.New("no phone")
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
	sessionDir := filepath.Join("session", sessionFolder(phone))
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

	// Обработчик новых сообщений
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok || msg.Out {
			return nil
		}

		// Проверка, что сообщение из рабочего чата
		if peerID, ok := msg.PeerID.(*tg.PeerChat); ok && peerID.ChatID == workChat {
			// Обработка аудиофайлов
			if media, ok := msg.Media.(*tg.MessageMediaDocument); ok {
				if doc, ok := media.Document.(*tg.Document); ok {
					if isAudioFile(doc) {
						fileName := getFileName(doc)
						if strings.HasSuffix(strings.ToLower(fileName), ".mp3") {
							// Обработка MP3
							downloadPath := fmt.Sprintf("downloads/%d.mp3", doc.ID)
							if _, err := downloadFile(api, doc, downloadPath); err != nil {
								return errors.Wrap(err, "download mp3")
							}
							oggPath := fmt.Sprintf("ogg_files/%d.ogg", doc.ID)
							if err := convertMp3ToOgg(downloadPath, oggPath); err != nil {
								return errors.Wrap(err, "convert mp3 to ogg")
							}
							if err := sendVoice(api, workChat, oggPath); err != nil {
								return errors.Wrap(err, "send voice")
							}
						} else if strings.HasSuffix(strings.ToLower(fileName), ".ogg") {
							// Обработка OGG
							oggPath := fmt.Sprintf("ogg_files/%d.ogg", doc.ID)
							if _, err := downloadFile(api, doc, oggPath); err != nil {
								return errors.Wrap(err, "download ogg")
							}
							if err := sendVoice(api, workChat, oggPath); err != nil {
								return errors.Wrap(err, "send voice")
							}
						}
					}
				}
			}

			// Обработка ответов на голосовые сообщения
			if {
				repliedMsg, err := getMessage(api, workChat, msg.ReplyTo)
				if err != nil {
					return errors.Wrap(err, "get replied message")
				}
				if repliedMedia, ok := repliedMsg.Media.(*tg.MessageMediaDocument); ok {
					if repliedDoc, ok := repliedMedia.Document.(*tg.Document); ok {
						if isVoiceMessage(repliedDoc) {
							if err := sendVoiceWithCaption(api, workChat, repliedDoc, msg.Message); err != nil {
								return errors.Wrap(err, "send voice with caption")
							}
						}
					}
				}
			}
		}
		return nil
	})

	// Аутентификация
	flow := auth.NewFlow(Terminal{PhoneNumber: phone}, auth.SendCodeOptions{})
	return waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			if err := client.Auth().IfNecessary(ctx, flow); err != nil {
				return errors.Wrap(err, "auth")
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
	location := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}
	d := downloader.NewDownloader()
	return d.Download(api, location).ToPath(context.Background(), path)
}

func convertMp3ToOgg(inputPath, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0700); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", "-i", inputPath, "-c:a", "libopus", outputPath)
	return cmd.Run()
}

func sendVoice(api *tg.Client, chatID int64, oggPath string) error {
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
	media := &tg.InputMediaUploadedDocument{
		File:       uploadedFile,
		Attributes: attributes,
	}

	_, err = api.MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{
		Peer:  &tg.InputPeerChat{ChatID: chatID},
		Media: media,
	})
	return err
}

func getMessage(api *tg.Client, chatID int64, msgID int) (*tg.Message, error) {
	resp, err := api.MessagesGetMessages(context.Background(), []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}})
	if err != nil {
		return nil, err
	}
	messages := resp.(*tg.MessagesMessages).GetMessages()
	for _, m := range messages {
		if msg, ok := m.(*tg.Message); ok {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message not found")
}

func sendVoiceWithCaption(api *tg.Client, chatID int64, doc *tg.Document, caption string) error {
	media := &tg.InputMediaDocument{
		ID: &tg.InputDocument{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		},
	}
	_, err := api.MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{
		Peer:    &tg.InputPeerChat{ChatID: chatID},
		Media:   media,
		Message: caption,
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
