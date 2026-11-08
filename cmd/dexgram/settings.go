package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dexgram/internal/codex"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	codexModelSettingKey     = "codex.model"
	codexReasoningSettingKey = "codex.reasoning_effort"
)

var reasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh"}

func (a *app) handleSettingsCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	text := a.settingsText(msg.Chat.ID, msg.MessageThreadID)
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              msg.Chat.ID,
		MessageThreadID:     msg.MessageThreadID,
		Text:                text,
		ReplyMarkup:         settingsMarkup(),
		DisableNotification: true,
	})
}

func (a *app) handleModelCommand(ctx context.Context, b *bot.Bot, msg *models.Message, arg string) {
	arg = strings.TrimSpace(arg)
	if arg != "" {
		a.setModelSetting(ctx, b, msg.Chat.ID, msg.MessageThreadID, arg, 0)
		return
	}
	text, markup := a.modelMenu(ctx, msg.Chat.ID, msg.MessageThreadID)
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              msg.Chat.ID,
		MessageThreadID:     msg.MessageThreadID,
		Text:                text,
		ReplyMarkup:         markup,
		DisableNotification: true,
	})
}

func (a *app) handleEffortCommand(ctx context.Context, b *bot.Bot, msg *models.Message, arg string) {
	arg = strings.TrimSpace(arg)
	if arg != "" {
		a.setReasoningSetting(ctx, b, msg.Chat.ID, msg.MessageThreadID, arg, 0)
		return
	}
	text, markup := a.reasoningMenu(msg.Chat.ID, msg.MessageThreadID)
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              msg.Chat.ID,
		MessageThreadID:     msg.MessageThreadID,
		Text:                text,
		ReplyMarkup:         markup,
		DisableNotification: true,
	})
}

func (a *app) settingsText(chatID int64, threadID int) string {
	model, _ := a.topicSetting(chatID, threadID, codexModelSettingKey)
	effort, _ := a.topicSetting(chatID, threadID, codexReasoningSettingKey)
	return strings.Join([]string{
		"Codex settings",
		"Model: " + settingLabel(model),
		"Reasoning effort: " + settingLabel(effort),
		"",
		"Used for new messages and /plan turns in this topic. Auto leaves the choice to Codex.",
	}, "\n")
}

func settingLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Auto"
	}
	return value
}

func settingsMarkup() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "Model", CallbackData: "settings:model"},
			{Text: "Reasoning", CallbackData: "settings:effort"},
		},
	}}
}

func (a *app) modelMenu(ctx context.Context, chatID int64, threadID int) (string, *models.InlineKeyboardMarkup) {
	current, _ := a.topicSetting(chatID, threadID, codexModelSettingKey)
	options, err := a.codexModels(ctx)
	if err != nil {
		return "Could not load Codex models:\n\n" + err.Error(), nil
	}
	rows := [][]models.InlineKeyboardButton{{{Text: selectedLabel("Auto", current == ""), CallbackData: "model:"}}}
	for _, option := range options {
		id := strings.TrimSpace(option.Name())
		if id == "" || len("model:"+id) > 64 {
			continue
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         selectedLabel(shortButtonLabel(id), id == current),
			CallbackData: "model:" + id,
		}})
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: "Settings", CallbackData: "settings:overview"}})
	return "Codex model\nCurrent: " + settingLabel(current), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func selectedLabel(label string, selected bool) string {
	if selected {
		return "* " + label
	}
	return label
}

func shortButtonLabel(label string) string {
	if len([]rune(label)) <= 48 {
		return label
	}
	runes := []rune(label)
	return string(runes[:45]) + "..."
}

func (a *app) reasoningMenu(chatID int64, threadID int) (string, *models.InlineKeyboardMarkup) {
	current, _ := a.topicSetting(chatID, threadID, codexReasoningSettingKey)
	rows := [][]models.InlineKeyboardButton{{{Text: selectedLabel("Auto", current == ""), CallbackData: "effort:"}}}
	for i := 0; i < len(reasoningEfforts); i += 2 {
		row := []models.InlineKeyboardButton{}
		for _, effort := range reasoningEfforts[i:min(i+2, len(reasoningEfforts))] {
			row = append(row, models.InlineKeyboardButton{
				Text:         selectedLabel(effort, effort == current),
				CallbackData: "effort:" + effort,
			})
		}
		rows = append(rows, row)
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: "Settings", CallbackData: "settings:overview"}})
	return "Codex reasoning effort\nCurrent: " + settingLabel(current), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *app) handleSettingsCallback(ctx context.Context, b *bot.Bot, query *models.CallbackQuery) {
	if query.Message.Message == nil {
		return
	}
	chatID := query.Message.Message.Chat.ID
	messageID := query.Message.Message.ID
	msg := query.Message.Message
	if !a.allowedChat(chatID) {
		return
	}
	action := strings.TrimPrefix(query.Data, "settings:")
	switch action {
	case "model":
		text, markup := a.modelMenu(ctx, chatID, msg.MessageThreadID)
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: text, ReplyMarkup: markup})
	case "effort":
		text, markup := a.reasoningMenu(chatID, msg.MessageThreadID)
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: text, ReplyMarkup: markup})
	default:
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: a.settingsText(chatID, msg.MessageThreadID), ReplyMarkup: settingsMarkup()})
	}
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
}

func (a *app) handleModelCallback(ctx context.Context, b *bot.Bot, query *models.CallbackQuery) {
	if query.Message.Message == nil {
		return
	}
	a.setModelSetting(ctx, b, query.Message.Message.Chat.ID, query.Message.Message.MessageThreadID, strings.TrimPrefix(query.Data, "model:"), query.Message.Message.ID)
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
}

func (a *app) handleEffortCallback(ctx context.Context, b *bot.Bot, query *models.CallbackQuery) {
	if query.Message.Message == nil {
		return
	}
	a.setReasoningSetting(ctx, b, query.Message.Message.Chat.ID, query.Message.Message.MessageThreadID, strings.TrimPrefix(query.Data, "effort:"), query.Message.Message.ID)
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
}

func (a *app) setModelSetting(ctx context.Context, b *bot.Bot, chatID int64, threadID int, value string, editMessageID int) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		value = ""
	}
	if value != "" {
		models, err := a.codexModels(ctx)
		if err != nil || !modelExists(models, value) {
			a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, "That model is not available. Use /model to refresh the list.", nil)
			return
		}
	}
	if err := a.saveTopicSetting(chatID, threadID, codexModelSettingKey, value); err != nil {
		a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, "Could not save model setting: "+err.Error(), nil)
		return
	}
	a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, a.settingsText(chatID, threadID)+"\n\nModel saved.", settingsMarkup())
}

func (a *app) setReasoningSetting(ctx context.Context, b *bot.Bot, chatID int64, threadID int, value string, editMessageID int) {
	value = normalizeReasoningEffort(value)
	if strings.EqualFold(value, "auto") {
		value = ""
	}
	if value != "" && !contains(reasoningEfforts, value) {
		a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, "Usage: /effort auto|minimal|low|medium|high|xhigh", nil)
		return
	}
	if err := a.saveTopicSetting(chatID, threadID, codexReasoningSettingKey, value); err != nil {
		a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, "Could not save reasoning effort: "+err.Error(), nil)
		return
	}
	a.sendOrEditPlain(ctx, b, chatID, threadID, editMessageID, a.settingsText(chatID, threadID)+"\n\nReasoning effort saved.", settingsMarkup())
}

func (a *app) sendOrEditPlain(ctx context.Context, b *bot.Bot, chatID int64, threadID int, editMessageID int, text string, markup *models.InlineKeyboardMarkup) {
	if editMessageID != 0 {
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: editMessageID, Text: text, ReplyMarkup: markup})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, MessageThreadID: threadID, Text: text, ReplyMarkup: markup, DisableNotification: true})
}

func (a *app) turnOptions(ctx context.Context, c *codex.Client, collaborationMode string, chatID int64, threadID int) (codexTurnOptions, error) {
	opts := codexTurnOptions{
		ApprovalPolicy:    a.cfg.Codex.ApprovalPolicy,
		Sandbox:           a.cfg.Codex.Sandbox,
		CollaborationMode: normalizeCollaborationMode(collaborationMode),
	}
	model, err := a.effectiveTopicSetting(chatID, threadID, codexModelSettingKey, opts.CollaborationMode != "")
	if err != nil {
		return opts, err
	}
	effort, err := a.effectiveTopicSetting(chatID, threadID, codexReasoningSettingKey, opts.CollaborationMode != "")
	if err != nil {
		return opts, err
	}
	opts.Model = strings.TrimSpace(model)
	opts.ReasoningEffort = normalizeReasoningEffort(effort)
	if opts.CollaborationMode != "" && opts.Model == "" {
		model, err := defaultCodexModel(ctx, c)
		if err != nil {
			return opts, err
		}
		opts.Model = model
	}
	return opts, nil
}

func defaultCodexModel(ctx context.Context, c *codex.Client) (string, error) {
	var out codex.ModelListResponse
	if err := c.Call(ctx, "model/list", map[string]any{}, &out); err != nil {
		return "", fmt.Errorf("load Codex models: %w", err)
	}
	for _, option := range out.Data {
		if option.IsDefault && strings.TrimSpace(option.Name()) != "" {
			return strings.TrimSpace(option.Name()), nil
		}
	}
	for _, option := range out.Data {
		if strings.TrimSpace(option.Name()) != "" && !option.Hidden {
			return strings.TrimSpace(option.Name()), nil
		}
	}
	return "", fmt.Errorf("codex model is required for plan mode; set one with /model")
}

func (a *app) codexModels(ctx context.Context) ([]codex.ModelOption, error) {
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: a.cfg.Codex.CWD,
	}, "models")
	if err != nil {
		return nil, err
	}
	var out codex.ModelListResponse
	if err := c.Call(ctx, "model/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	options := make([]codex.ModelOption, 0, len(out.Data))
	for _, option := range out.Data {
		if strings.TrimSpace(option.Name()) == "" || option.Hidden {
			continue
		}
		options = append(options, option)
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].IsDefault != options[j].IsDefault {
			return options[i].IsDefault
		}
		return options[i].Name() < options[j].Name()
	})
	return options, nil
}

func modelExists(options []codex.ModelOption, value string) bool {
	for _, option := range options {
		if option.Name() == value {
			return true
		}
	}
	return false
}

func normalizeCollaborationMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "plan", "plan_mode", "plan-mode":
		return "plan"
	default:
		return ""
	}
}

func normalizeReasoningEffort(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "x-high", "x_high", "extra-high", "extra_high":
		return "xhigh"
	default:
		return strings.TrimSpace(strings.ToLower(value))
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func topicSettingKey(chatID int64, threadID int, key string) string {
	return fmt.Sprintf("topic:%d:%d:%s", chatID, threadID, key)
}

func (a *app) saveTopicSetting(chatID int64, threadID int, key, value string) error {
	if value == "" {
		value = "auto"
	}
	return a.store.SetSetting(topicSettingKey(chatID, threadID, key), value)
}

func (a *app) topicSetting(chatID int64, threadID int, key string) (string, error) {
	return a.effectiveTopicSetting(chatID, threadID, key, false)
}

func (a *app) effectiveTopicSetting(chatID int64, threadID int, key string, plan bool) (string, error) {
	value, err := a.store.GetSetting(topicSettingKey(chatID, threadID, key))
	if err != nil || value == "auto" {
		return "", err
	}
	if value == "" && plan {
		return a.store.GetSetting(key)
	}
	return value, nil
}
