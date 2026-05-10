package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/mcstatus-io/mcutil/v4/response"
	"github.com/mcstatus-io/mcutil/v4/status"
)

type config struct {
	host              string
	port              uint16
	pollInterval      time.Duration
	requestTimeout    time.Duration
	runOnce           bool
	notifyOnStart     bool
	serverEdition     string
	allowLegacyStatus bool
	telegramToken     string
	telegramChatID    string
	telegramParseMode string
	telegramUsePhoto  bool
	disableWebPreview bool
	includeMotd       bool
	includePlayers    bool
	includeVersion    bool
	includeAddress    bool
	messagePrefix     string
	openText          string
	closedText        string
}

type serverStatus struct {
	kind          string
	version       string
	protocol      string
	motd          string
	playersOnline *int64
	playersMax    *int64
	samplePlayers []string
	latency       *time.Duration
	modsCount     *int
	gamemode      string
	serverID      string
	iconData      string
}

func main() {
	_ = godotenv.Load()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	address := net.JoinHostPort(cfg.host, strconv.Itoa(int(cfg.port)))
	log.Printf("monitoring %s", address)

	lastStatus := -1
	for {
		status, statusOK, statusErr := fetchMinecraftStatus(cfg)
		if statusErr != nil {
			log.Printf("status error: %v", statusErr)
		}

		isOpen := statusOK

		if lastStatus == -1 {
			log.Printf("initial status: %s", statusLabel(isOpen))
			if cfg.notifyOnStart {
				message := buildMessage(cfg, address, isOpen, statusOK, status)
				if err := sendTelegramNotification(cfg, message, statusOK, status); err != nil {
					log.Printf("telegram error: %v", err)
				}
			}
		} else if (isOpen && lastStatus == 0) || (!isOpen && lastStatus == 1) {
			message := buildMessage(cfg, address, isOpen, statusOK, status)
			if err := sendTelegramNotification(cfg, message, statusOK, status); err != nil {
				log.Printf("telegram error: %v", err)
			}
		}

		if isOpen {
			lastStatus = 1
		} else {
			lastStatus = 0
		}

		if cfg.runOnce {
			return
		}
		time.Sleep(cfg.pollInterval)
	}
}

func loadConfig() (config, error) {
	cfg := config{}

	cfg.host = strings.TrimSpace(os.Getenv("SERVER_HOST"))
	portRaw := strings.TrimSpace(os.Getenv("SERVER_PORT"))
	if cfg.host == "" || portRaw == "" {
		return cfg, fmt.Errorf("SERVER_HOST and SERVER_PORT are required")
	}

	port, err := parsePort(portRaw)
	if err != nil {
		return cfg, err
	}
	cfg.port = port

	pollSec := getEnvInt("POLL_INTERVAL_SEC", 30)
	if pollSec < 1 {
		return cfg, fmt.Errorf("POLL_INTERVAL_SEC must be >= 1")
	}
	cfg.pollInterval = time.Duration(pollSec) * time.Second

	timeoutSec := getEnvInt("REQUEST_TIMEOUT_SEC", 3)
	if timeoutSec < 1 {
		return cfg, fmt.Errorf("REQUEST_TIMEOUT_SEC must be >= 1")
	}
	cfg.requestTimeout = time.Duration(timeoutSec) * time.Second

	cfg.runOnce = getEnvBool("RUN_ONCE", false)
	cfg.notifyOnStart = getEnvBool("NOTIFY_ON_START", false)

	cfg.serverEdition = strings.ToLower(strings.TrimSpace(os.Getenv("SERVER_EDITION")))
	if cfg.serverEdition == "" {
		cfg.serverEdition = "java"
	}
	if cfg.serverEdition != "java" && cfg.serverEdition != "bedrock" {
		return cfg, fmt.Errorf("SERVER_EDITION must be java or bedrock")
	}
	cfg.allowLegacyStatus = getEnvBool("ALLOW_LEGACY_STATUS", true)

	cfg.telegramToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	cfg.telegramChatID = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	if cfg.telegramToken == "" || cfg.telegramChatID == "" {
		return cfg, fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
	}
	cfg.telegramParseMode = strings.TrimSpace(os.Getenv("TELEGRAM_PARSE_MODE"))
	if cfg.telegramParseMode == "" {
		cfg.telegramParseMode = "HTML"
	}
	cfg.telegramUsePhoto = getEnvBool("TELEGRAM_USE_PHOTO", true)
	cfg.disableWebPreview = getEnvBool("DISABLE_WEB_PREVIEW", true)

	cfg.includeMotd = getEnvBool("INCLUDE_MOTD", true)
	cfg.includePlayers = getEnvBool("INCLUDE_PLAYERS", true)
	cfg.includeVersion = getEnvBool("INCLUDE_VERSION", true)
	cfg.includeAddress = getEnvBool("INCLUDE_ADDRESS", true)

	cfg.messagePrefix = strings.TrimSpace(os.Getenv("MESSAGE_PREFIX"))
	if cfg.messagePrefix == "" {
		cfg.messagePrefix = "Minecraft server"
	}
	cfg.openText = strings.TrimSpace(os.Getenv("OPEN_TEXT"))
	if cfg.openText == "" {
		cfg.openText = "OPEN"
	}
	cfg.closedText = strings.TrimSpace(os.Getenv("CLOSED_TEXT"))
	if cfg.closedText == "" {
		cfg.closedText = "CLOSED"
	}

	return cfg, nil
}

func getEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func getEnvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "t", "yes", "y":
		return true
	case "0", "false", "f", "no", "n":
		return false
	default:
		return fallback
	}
}

func parsePort(value string) (uint16, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("SERVER_PORT must be between 1 and 65535")
	}
	return uint16(port), nil
}

func fetchMinecraftStatus(cfg config) (serverStatus, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.requestTimeout)
	defer cancel()

	switch cfg.serverEdition {
	case "java":
		return fetchJavaStatus(ctx, cfg)
	case "bedrock":
		return fetchBedrockStatus(ctx, cfg)
	default:
		return serverStatus{}, false, fmt.Errorf("SERVER_EDITION must be java or bedrock")
	}
}

func fetchJavaStatus(ctx context.Context, cfg config) (serverStatus, bool, error) {
	modern, err := status.Modern(ctx, cfg.host, cfg.port)
	if err == nil {
		return mapModernStatus(modern), true, nil
	}
	if !cfg.allowLegacyStatus {
		return serverStatus{}, false, err
	}
	legacy, legacyErr := status.Legacy(ctx, cfg.host, cfg.port)
	if legacyErr == nil {
		return mapLegacyStatus(legacy), true, nil
	}
	return serverStatus{}, false, fmt.Errorf("modern status error: %v; legacy status error: %v", err, legacyErr)
}

func fetchBedrockStatus(ctx context.Context, cfg config) (serverStatus, bool, error) {
	bedrock, err := status.Bedrock(ctx, cfg.host, cfg.port)
	if err != nil {
		return serverStatus{}, false, err
	}
	return mapBedrockStatus(bedrock), true, nil
}

func mapModernStatus(modern *response.StatusModern) serverStatus {
	result := serverStatus{
		kind:          "Java (modern)",
		version:       modern.Version.Name.Clean,
		protocol:      fmt.Sprintf("%d", modern.Version.Protocol),
		motd:          modern.MOTD.Clean,
		playersOnline: modern.Players.Online,
		playersMax:    modern.Players.Max,
	}
	latency := modern.Latency
	result.latency = &latency
	if modern.Favicon != nil {
		result.iconData = *modern.Favicon
	}
	if modern.Mods != nil {
		count := len(modern.Mods.List)
		result.modsCount = &count
	}
	if len(modern.Players.Sample) > 0 {
		for _, sample := range modern.Players.Sample {
			name := strings.TrimSpace(sample.Name.Clean)
			if name != "" {
				result.samplePlayers = append(result.samplePlayers, name)
			}
		}
	}
	return result
}

func mapLegacyStatus(legacy *response.StatusLegacy) serverStatus {
	result := serverStatus{
		kind: "Java (legacy)",
		motd: legacy.MOTD.Clean,
	}
	if legacy.Version != nil {
		result.version = legacy.Version.Name.Clean
		result.protocol = fmt.Sprintf("%d", legacy.Version.Protocol)
	}
	online := legacy.Players.Online
	max := legacy.Players.Max
	result.playersOnline = &online
	result.playersMax = &max
	return result
}

func mapBedrockStatus(bedrock *response.StatusBedrock) serverStatus {
	result := serverStatus{
		kind: "Bedrock",
	}
	if bedrock.Edition != nil && *bedrock.Edition != "" {
		result.kind = fmt.Sprintf("Bedrock (%s)", *bedrock.Edition)
	}
	if bedrock.MOTD != nil {
		result.motd = bedrock.MOTD.Clean
	}
	if bedrock.Version != nil {
		result.version = *bedrock.Version
	}
	if bedrock.ProtocolVersion != nil {
		result.protocol = fmt.Sprintf("%d", *bedrock.ProtocolVersion)
	}
	if bedrock.OnlinePlayers != nil {
		result.playersOnline = bedrock.OnlinePlayers
	}
	if bedrock.MaxPlayers != nil {
		result.playersMax = bedrock.MaxPlayers
	}
	if bedrock.Gamemode != nil {
		result.gamemode = *bedrock.Gamemode
	}
	if bedrock.ServerID != nil {
		result.serverID = *bedrock.ServerID
	}
	return result
}

func sendTelegramNotification(cfg config, message string, statusOK bool, status serverStatus) error {
	if cfg.telegramUsePhoto && statusOK {
		if iconBytes, ok := decodeIcon(status.iconData); ok {
			return sendTelegramPhoto(cfg, message, iconBytes)
		}
	}
	return sendTelegramMessage(cfg, message)
}

func sendTelegramMessage(cfg config, message string) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.telegramToken)
	form := url.Values{}
	form.Set("chat_id", cfg.telegramChatID)
	form.Set("text", message)
	form.Set("parse_mode", cfg.telegramParseMode)
	if cfg.disableWebPreview {
		form.Set("disable_web_page_preview", "true")
	}

	client := &http.Client{Timeout: cfg.requestTimeout}
	resp, err := client.PostForm(endpoint, form)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram status: %s", resp.Status)
	}
	return nil
}

func sendTelegramPhoto(cfg config, caption string, photo []byte) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", cfg.telegramToken)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("chat_id", cfg.telegramChatID); err != nil {
		return err
	}
	if err := writer.WriteField("caption", caption); err != nil {
		return err
	}
	if err := writer.WriteField("parse_mode", cfg.telegramParseMode); err != nil {
		return err
	}

	part, err := writer.CreateFormFile("photo", "server.png")
	if err != nil {
		return err
	}
	if _, err := part.Write(photo); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	client := &http.Client{Timeout: cfg.requestTimeout}
	req, err := http.NewRequest("POST", endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram status: %s", resp.Status)
	}
	return nil
}

func decodeIcon(icon string) ([]byte, bool) {
	if icon == "" {
		return nil, false
	}
	comma := strings.Index(icon, ",")
	if comma == -1 {
		return nil, false
	}
	data := icon[comma+1:]
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func buildMessage(cfg config, address string, isOpen bool, statusOK bool, status serverStatus) string {
	statusText := cfg.closedText
	if isOpen {
		statusText = cfg.openText
	}

	lines := []string{
		fmt.Sprintf("<b>%s</b>", escapeHTML(cfg.messagePrefix)),
		fmt.Sprintf("Status: <b>%s</b>", escapeHTML(statusText)),
	}

	if cfg.includeAddress {
		lines = append(lines, fmt.Sprintf("Address: <code>%s</code>", escapeHTML(address)))
	}

	if statusOK {
		if status.kind != "" {
			lines = append(lines, fmt.Sprintf("Type: <code>%s</code>", escapeHTML(status.kind)))
		}
		if cfg.includeVersion && status.version != "" {
			lines = append(lines, fmt.Sprintf("Version: <code>%s</code>", escapeHTML(status.version)))
		}
		if cfg.includeVersion && status.protocol != "" {
			lines = append(lines, fmt.Sprintf("Protocol: <code>%s</code>", escapeHTML(status.protocol)))
		}
		if cfg.includePlayers && status.playersOnline != nil && status.playersMax != nil {
			lines = append(lines, fmt.Sprintf("Players: <code>%d/%d</code>", *status.playersOnline, *status.playersMax))
		}
		if cfg.includePlayers && len(status.samplePlayers) > 0 {
			lines = append(lines, fmt.Sprintf("Online: <code>%s</code>", escapeHTML(strings.Join(status.samplePlayers, ", "))))
		}
		if cfg.includeMotd && status.motd != "" {
			lines = append(lines, fmt.Sprintf("MOTD: <i>%s</i>", escapeHTML(status.motd)))
		}
		if status.latency != nil {
			lines = append(lines, fmt.Sprintf("Latency: <code>%s</code>", escapeHTML(status.latency.String())))
		}
		if status.modsCount != nil {
			lines = append(lines, fmt.Sprintf("Mods: <code>%d</code>", *status.modsCount))
		}
		if status.gamemode != "" {
			lines = append(lines, fmt.Sprintf("Gamemode: <code>%s</code>", escapeHTML(status.gamemode)))
		}
		if status.serverID != "" {
			lines = append(lines, fmt.Sprintf("Server ID: <code>%s</code>", escapeHTML(status.serverID)))
		}
	}

	return strings.Join(lines, "\n")
}

func escapeHTML(input string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(input)
}

func statusLabel(isOpen bool) string {
	if isOpen {
		return "open"
	}
	return "closed"
}
