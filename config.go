package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config mirrors WebStreamer/vars.py, trimmed to what start+stream needs.
// (no MULTI_TOKEN pool, no fsub, no mongo user-db, no admin/broadcast —
// dropped per scope decision.)
type Config struct {
	APIID                int
	APIHash              string
	BotToken             string
	BinChannel           int64 // as given, e.g. -1001234567890
	BinChannelAccessHash int64 // optional: skip discovery entirely if set
	Port                 string
	BindAddr             string
	HashLength           int
	FQDN                 string
	HasSSL               bool
	NoPort               bool
	URL                  string
	SessionFile          string
	BinChannelCache      string // where the resolved access_hash gets persisted across restarts
	ChannelURL           string // optional: shown as a button on /start

	FsubChannel           int64  // 0 = fsub disabled
	FsubChannelAccessHash int64  // optional: skip discovery entirely if set
	FsubChannelURL        string // shown as the "join" button when gating
	FsubChannelCache      string // persisted access_hash cache, same format as BinChannelCache

	PerStreamParallel int // how many chunks one HTTP stream prefetches at once
	MaxConcurrentDL   int // global cap on in-flight upload.getFile calls
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", key)
		os.Exit(1)
	}
	return v
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "t" || v == "yes" || v == "y"
}

func loadConfig() Config {
	apiID, err := strconv.Atoi(mustEnv("API_ID"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "API_ID must be an int")
		os.Exit(1)
	}
	binChannel, err := strconv.ParseInt(mustEnv("BIN_CHANNEL"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "BIN_CHANNEL must be an int (e.g. -1001234567890)")
		os.Exit(1)
	}
	hashLen, _ := strconv.Atoi(envDefault("HASH_LENGTH", "6"))
	if hashLen <= 5 || hashLen >= 64 {
		fmt.Fprintln(os.Stderr, "HASH_LENGTH should be greater than 5 and less than 64")
		os.Exit(1)
	}
	port := envDefault("PORT", "8080")
	bindAddr := envDefault("WEB_SERVER_BIND_ADDRESS", "0.0.0.0")
	hasSSL := envBool("HAS_SSL", true)
	noPort := envBool("NO_PORT", true)
	fqdn := envDefault("FQDN", bindAddr)

	scheme := "http"
	if hasSSL {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s", scheme, fqdn)
	if !noPort {
		url += ":" + port
	}
	url += "/"

	sessionFile := envDefault("SESSION_FILE", "gofilestream.session.json")
	var accessHash int64
	if v := os.Getenv("BIN_CHANNEL_ACCESS_HASH"); v != "" {
		accessHash, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "BIN_CHANNEL_ACCESS_HASH must be an int")
			os.Exit(1)
		}
	}
	var fsubChannel, fsubAccessHash int64
	if v := os.Getenv("FSUB_CHANNEL"); v != "" {
		fsubChannel, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "FSUB_CHANNEL must be an int (e.g. -1001234567890)")
			os.Exit(1)
		}
	}
	if v := os.Getenv("FSUB_CHANNEL_ACCESS_HASH"); v != "" {
		fsubAccessHash, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "FSUB_CHANNEL_ACCESS_HASH must be an int")
			os.Exit(1)
		}
	}
	return Config{
		APIID:                apiID,
		APIHash:              mustEnv("API_HASH"),
		BotToken:             mustEnv("BOT_TOKEN"),
		BinChannel:           binChannel,
		BinChannelAccessHash: accessHash,
		Port:                 port,
		BindAddr:             bindAddr,
		HashLength:           hashLen,
		FQDN:                 fqdn,
		HasSSL:               hasSSL,
		NoPort:               noPort,
		URL:                  url,
		SessionFile:          sessionFile,
		BinChannelCache:      envDefault("BIN_CHANNEL_CACHE_FILE", sessionFile+".binchannel.json"),
		ChannelURL:           os.Getenv("CHANNEL_URL"),

		FsubChannel:           fsubChannel,
		FsubChannelAccessHash: fsubAccessHash,
		FsubChannelURL:        os.Getenv("FSUB_CHANNEL_URL"),
		FsubChannelCache:      envDefault("FSUB_CHANNEL_CACHE_FILE", sessionFile+".fsubchannel.json"),

		PerStreamParallel: envInt("PER_STREAM_PARALLEL", 24),
		MaxConcurrentDL:   envInt("MAX_CONCURRENT_DOWNLOADS", 32),
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// rawChannelID converts a bot-API style channel id (-100xxxxxxxxxx) to the
// raw MTProto channel id used in tg.InputChannel. Accepts already-raw
// positive ids too.
func rawChannelID(id int64) int64 {
	s := strconv.FormatInt(id, 10)
	if strings.HasPrefix(s, "-100") {
		raw, _ := strconv.ParseInt(strings.TrimPrefix(s, "-100"), 10, 64)
		return raw
	}
	if id < 0 {
		return -id
	}
	return id
}
