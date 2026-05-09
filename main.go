package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
)

type config struct {
	host              string
	port              string
	pollInterval      time.Duration
	requestTimeout    time.Duration
	runOnce           bool
	notifyOnStart     bool
	telegramToken     string
	telegramChatID    string
	telegramParseMode string
	telegramUsePhoto  bool
	disableWebPreview bool
	statusAPIEnabled  bool
	statusAPIBase     string
	includeMotd       bool
	includePlayers    bool
	includeVersion    bool
	includeAddress    bool
	messagePrefix     string
	openText          string
	closedText        string
}

type mcStatus struct {
	Online  bool   `json:"online"`
	Version string `json:"version"`
	Icon    string `json:"icon"`
	Motd    struct {
		Clean []string `json:"clean"`
	} `json:"motd"`
	Players struct {
		Online int `json:"online"`
		Max    int `json:"max"`
	} `json:"players"`
}

func main() {
	_ = godotenv.Load()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	address := net.JoinHostPort(cfg.host, cfg.port)
	log.Printf("monitoring %s", address)

	lastStatus := -1
	for {
		status, statusOK, statusErr := fetchStatus(cfg, address)
		if statusErr != nil {
			log.Printf("status api error: %v", statusErr)
		}

		isOpen := false
		if statusOK {
			isOpen = status.Online
		} else {
			isOpen = checkOpen(address, cfg.requestTimeout)
		}

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
	cfg.port = strings.TrimSpace(os.Getenv("SERVER_PORT"))
	if cfg.host == "" || cfg.port == "" {
		return cfg, fmt.Errorf("SERVER_HOST and SERVER_PORT are required")
	}

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

	cfg.statusAPIEnabled = getEnvBool("STATUS_API_ENABLED", true)
	cfg.statusAPIBase = strings.TrimSpace(os.Getenv("STATUS_API_BASE"))
	if cfg.statusAPIBase == "" {
		cfg.statusAPIBase = "https://api.mcsrvstat.us/2"
	}
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

func checkOpen(address string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func fetchStatus(cfg config, address string) (mcStatus, bool, error) {
	if !cfg.statusAPIEnabled {
		return mcStatus{}, false, nil
	}
	base := strings.TrimRight(cfg.statusAPIBase, "/")
	endpoint := fmt.Sprintf("%s/%s", base, address)

	client := &http.Client{Timeout: cfg.requestTimeout}
	resp, err := client.Get(endpoint)
	if err != nil {
		return mcStatus{}, false, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcStatus{}, false, fmt.Errorf("status api status: %s", resp.Status)
	}

	var status mcStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return mcStatus{}, false, err
	}
	return status, true, nil
}

func sendTelegramNotification(cfg config, message string, statusOK bool, status mcStatus) error {
	if cfg.telegramUsePhoto && statusOK {
		if iconBytes, ok := decodeIcon(status.Icon); ok {
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

func buildMessage(cfg config, address string, isOpen bool, statusOK bool, status mcStatus) string {
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
		if cfg.includeVersion && status.Version != "" {
			lines = append(lines, fmt.Sprintf("Version: <code>%s</code>", escapeHTML(status.Version)))
		}
		if cfg.includePlayers && status.Players.Max > 0 {
			lines = append(lines, fmt.Sprintf("Players: <code>%d/%d</code>", status.Players.Online, status.Players.Max))
		}
		if cfg.includeMotd && len(status.Motd.Clean) > 0 {
			motd := strings.Join(status.Motd.Clean, " | ")
			lines = append(lines, fmt.Sprintf("MOTD: <i>%s</i>", escapeHTML(motd)))
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
