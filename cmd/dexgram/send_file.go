package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const maxTelegramUpload = 50 * 1024 * 1024

func (a *app) handleSendFileCommand(ctx context.Context, b *bot.Bot, msg *models.Message, arg string) {
	reply := func(text string) { a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, 0, text, nil) }
	if msg.Chat.Type != models.ChatTypePrivate {
		reply("File sending is available in a private Dexgram chat.")
		return
	}
	arg = strings.Trim(strings.TrimSpace(arg), `"'`)
	if arg == "" {
		reply("Use /sendfile <path> with a file in this topic's workspace.")
		return
	}
	conv, ok, err := a.store.Get(msg.Chat.ID, msg.MessageThreadID)
	if err != nil || !ok || conv.CWD == "" {
		reply("Attach or start a Codex chat with a workspace first.")
		return
	}
	file, err := openWorkspaceFile(conv.CWD, arg)
	if err != nil {
		reply(err.Error())
		return
	}
	defer func() { _ = file.Close() }()
	if err := sendTelegramFile(ctx, b, msg.Chat.ID, msg.MessageThreadID, file, false); err != nil {
		log.Printf("send requested workspace file: %v", err)
		reply("Telegram could not send the file. Try again.")
	}
}

func openWorkspaceFile(cwd, path string) (*os.File, error) {
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("This topic has no local workspace.")
	}
	if filepath.IsAbs(path) {
		relative, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil, fmt.Errorf("Choose a file within this topic's workspace.")
		}
		path = relative
	}
	if !filepath.IsLocal(path) || strings.Contains(path, ":") {
		return nil, fmt.Errorf("Choose a file within this topic's workspace.")
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, fmt.Errorf("This topic's workspace is unavailable.")
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Could not open that file within this topic's workspace.")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTelegramUpload {
		_ = file.Close()
		return nil, fmt.Errorf("Choose a regular file no larger than 50 MB.")
	}
	return file, nil
}

func sendTelegramFile(ctx context.Context, b *bot.Bot, chatID int64, topic int, file *os.File, photo bool) error {
	name := filepath.Base(file.Name())
	upload := &models.InputFileUpload{Filename: name, Data: file}
	if photo && isImagePath(name) {
		_, err := b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: chatID, MessageThreadID: topic, Photo: upload, DisableNotification: true})
		return err
	}
	_, err := b.SendDocument(ctx, &bot.SendDocumentParams{ChatID: chatID, MessageThreadID: topic, Document: upload, DisableNotification: true})
	return err
}
