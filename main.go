package main

import (
	"bytes"
	cloudpubsub "cloud.google.com/go/pubsub"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	projectID          string
	subscriptionID     string
	deadLetterTopicID  string
	backendURL         string
	workerSharedToken  string
	requestTimeout     time.Duration
	maxHTTPRetries     int
	backoffInitial     time.Duration
	backoffMax         time.Duration
	maxDeliveryAttempt int
	port               string
}

type gmailNotification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

func (g *gmailNotification) UnmarshalJSON(data []byte) error {
	type rawNotification struct {
		EmailAddress string          `json:"emailAddress"`
		HistoryID    json.RawMessage `json:"historyId"`
	}

	var raw rawNotification
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	historyID, err := normalizeHistoryID(raw.HistoryID)
	if err != nil {
		return fmt.Errorf("invalid historyId: %w", err)
	}

	g.EmailAddress = strings.TrimSpace(raw.EmailAddress)
	g.HistoryID = historyID
	return nil
}

func normalizeHistoryID(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("missing")
	}

	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", errors.New("empty string")
		}
		return value, nil
	}

	var value json.Number
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", err
	}

	normalized := strings.TrimSpace(value.String())
	if normalized == "" {
		return "", errors.New("empty number")
	}

	return normalized, nil
}

type deadLetterEnvelope struct {
	Source            string            `json:"source"`
	OriginalMessageID string            `json:"originalMessageId"`
	DeliveryAttempt   int               `json:"deliveryAttempt"`
	Reason            string            `json:"reason"`
	RawPayload        string            `json:"rawPayload"`
	Attributes        map[string]string `json:"attributes,omitempty"`
	CreatedAt         string            `json:"createdAt"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client, err := cloudpubsub.NewClient(ctx, cfg.projectID)
	if err != nil {
		log.Fatalf("pubsub client error: %v", err)
	}
	defer client.Close()

	sub := client.Subscription(cfg.subscriptionID)
	sub.ReceiveSettings.MaxOutstandingMessages = 10
	sub.ReceiveSettings.NumGoroutines = 4
	sub.ReceiveSettings.Synchronous = false
	sub.ReceiveSettings.MaxExtension = 30 * time.Minute

	httpClient := &http.Client{
		Timeout: cfg.requestTimeout,
	}

	var deadLetterTopic *cloudpubsub.Topic
	if cfg.deadLetterTopicID != "" {
		deadLetterTopic = client.Topic(cfg.deadLetterTopicID)
		defer deadLetterTopic.Stop()
	}

	go runHealthServer(ctx, cfg.port)

	log.Printf("listening on Pub/Sub subscription=%s project=%s", cfg.subscriptionID, cfg.projectID)
	err = sub.Receive(ctx, func(ctx context.Context, msg *cloudpubsub.Message) {
		deliveryAttempt := getDeliveryAttempt(msg)

		var payload gmailNotification
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			log.Printf("invalid pubsub payload message_id=%s err=%v", msg.ID, err)
			handleFailedMessage(ctx, msg, deadLetterTopic, deliveryAttempt, cfg, msg.Data, err)
			return
		}

		if payload.EmailAddress == "" || payload.HistoryID == "" {
			log.Printf("missing emailAddress/historyId message_id=%s", msg.ID)
			handleFailedMessage(
				ctx,
				msg,
				deadLetterTopic,
				deliveryAttempt,
				cfg,
				msg.Data,
				errors.New("missing emailAddress/historyId"),
			)
			return
		}

		if err := sendToBackendWithRetry(ctx, httpClient, cfg, payload, msg.ID); err != nil {
			log.Printf("backend call failed message_id=%s err=%v", msg.ID, err)
			handleFailedMessage(ctx, msg, deadLetterTopic, deliveryAttempt, cfg, msg.Data, err)
			return
		}

		msg.Ack()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("pubsub receive failed: %v", err)
	}

	log.Println("worker stopped")
}

func loadConfig() (config, error) {
	cfg := config{
		projectID:          strings.TrimSpace(os.Getenv("GCP_PROJECT_ID")),
		subscriptionID:     strings.TrimSpace(os.Getenv("GMAIL_PUBSUB_SUBSCRIPTION")),
		deadLetterTopicID:  strings.TrimSpace(os.Getenv("GMAIL_DEAD_LETTER_TOPIC")),
		backendURL:         strings.TrimSpace(os.Getenv("PAYLOAD_INFO_BOT_ENDPOINT_URL")),
		workerSharedToken:  strings.TrimSpace(os.Getenv("GMAIL_WORKER_SHARED_SECRET")),
		requestTimeout:     12 * time.Second,
		maxHTTPRetries:     4,
		backoffInitial:     500 * time.Millisecond,
		backoffMax:         8 * time.Second,
		maxDeliveryAttempt: 12,
		port:               strings.TrimSpace(os.Getenv("PORT")),
	}

	if cfg.port == "" {
		cfg.port = "8080"
	}

	timeoutSeconds, err := loadIntEnv("HTTP_TIMEOUT_SECONDS", 12)
	if err != nil {
		return config{}, err
	}
	cfg.requestTimeout = time.Duration(timeoutSeconds) * time.Second

	maxHTTPRetries, err := loadIntEnv("MAX_HTTP_RETRIES", 4)
	if err != nil {
		return config{}, err
	}
	if maxHTTPRetries < 1 {
		return config{}, errors.New("MAX_HTTP_RETRIES must be >= 1")
	}
	cfg.maxHTTPRetries = maxHTTPRetries

	backoffInitialMS, err := loadIntEnv("BACKOFF_INITIAL_MS", 500)
	if err != nil {
		return config{}, err
	}
	backoffMaxMS, err := loadIntEnv("BACKOFF_MAX_MS", 8000)
	if err != nil {
		return config{}, err
	}
	if backoffInitialMS < 1 || backoffMaxMS < 1 {
		return config{}, errors.New("BACKOFF_INITIAL_MS and BACKOFF_MAX_MS must be >= 1")
	}
	if backoffInitialMS > backoffMaxMS {
		return config{}, errors.New("BACKOFF_INITIAL_MS cannot be greater than BACKOFF_MAX_MS")
	}
	cfg.backoffInitial = time.Duration(backoffInitialMS) * time.Millisecond
	cfg.backoffMax = time.Duration(backoffMaxMS) * time.Millisecond

	maxDeliveryAttempt, err := loadIntEnv("MAX_DELIVERY_ATTEMPT", 12)
	if err != nil {
		return config{}, err
	}
	if maxDeliveryAttempt < 1 {
		return config{}, errors.New("MAX_DELIVERY_ATTEMPT must be >= 1")
	}
	cfg.maxDeliveryAttempt = maxDeliveryAttempt

	switch {
	case cfg.projectID == "":
		return config{}, errors.New("GCP_PROJECT_ID is required")
	case cfg.subscriptionID == "":
		return config{}, errors.New("GMAIL_PUBSUB_SUBSCRIPTION is required")
	case cfg.backendURL == "":
		return config{}, errors.New("PAYLOAD_INFO_BOT_ENDPOINT_URL is required")
	case cfg.workerSharedToken == "":
		return config{}, errors.New("GMAIL_WORKER_SHARED_SECRET is required")
	default:
		return cfg, nil
	}
}

func loadIntEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value: %s", name, raw)
	}
	return parsed, nil
}

func getDeliveryAttempt(msg *cloudpubsub.Message) int {
	if msg.DeliveryAttempt == nil {
		return 1
	}
	return *msg.DeliveryAttempt
}

func sendToBackendWithRetry(
	ctx context.Context,
	httpClient *http.Client,
	cfg config,
	payload gmailNotification,
	messageID string,
) error {
	var lastErr error
	for attempt := 1; attempt <= cfg.maxHTTPRetries; attempt++ {
		lastErr = sendToBackend(ctx, httpClient, cfg, payload, messageID)
		if lastErr == nil {
			return nil
		}

		if attempt == cfg.maxHTTPRetries {
			break
		}
		backoff := calculateBackoff(cfg, attempt)
		log.Printf(
			"retry backend call message_id=%s attempt=%d/%d backoff=%s err=%v",
			messageID,
			attempt+1,
			cfg.maxHTTPRetries,
			backoff,
			lastErr,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return lastErr
}

func calculateBackoff(cfg config, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	multiplier := math.Pow(2, float64(attempt-1))
	backoff := time.Duration(float64(cfg.backoffInitial) * multiplier)
	if backoff > cfg.backoffMax {
		return cfg.backoffMax
	}
	return backoff
}

func sendToBackend(
	ctx context.Context,
	httpClient *http.Client,
	cfg config,
	payload gmailNotification,
	messageID string,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.backendURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.workerSharedToken)
	req.Header.Set("X-Pubsub-Message-Id", messageID)

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post backend: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		resBody, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("backend status=%d body=%s", res.StatusCode, string(resBody))
	}

	return nil
}

func handleFailedMessage(
	ctx context.Context,
	msg *cloudpubsub.Message,
	deadLetterTopic *cloudpubsub.Topic,
	deliveryAttempt int,
	cfg config,
	rawPayload []byte,
	cause error,
) {
	if shouldDeadLetter(deliveryAttempt, cfg) {
		if deadLetterTopic == nil {
			log.Printf(
				"dead-letter skipped (topic not configured) message_id=%s attempt=%d",
				msg.ID,
				deliveryAttempt,
			)
			msg.Nack()
			return
		}
		if err := publishDeadLetter(ctx, deadLetterTopic, msg, rawPayload, deliveryAttempt, cause); err != nil {
			log.Printf("dead-letter publish failed message_id=%s err=%v", msg.ID, err)
			msg.Nack()
			return
		}
		log.Printf(
			"message moved to dead-letter message_id=%s attempt=%d topic=%s",
			msg.ID,
			deliveryAttempt,
			cfg.deadLetterTopicID,
		)
		msg.Ack()
		return
	}
	msg.Nack()
}

func shouldDeadLetter(deliveryAttempt int, cfg config) bool {
	return cfg.deadLetterTopicID != "" && deliveryAttempt >= cfg.maxDeliveryAttempt
}

func publishDeadLetter(
	ctx context.Context,
	topic *cloudpubsub.Topic,
	msg *cloudpubsub.Message,
	rawPayload []byte,
	deliveryAttempt int,
	cause error,
) error {
	envelope := deadLetterEnvelope{
		Source:            "gmail-worker-migla",
		OriginalMessageID: msg.ID,
		DeliveryAttempt:   deliveryAttempt,
		Reason:            cause.Error(),
		RawPayload:        string(rawPayload),
		Attributes:        msg.Attributes,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal dead-letter envelope: %w", err)
	}

	result := topic.Publish(ctx, &cloudpubsub.Message{
		Data: data,
		Attributes: map[string]string{
			"source":              "gmail-worker-migla",
			"original_message_id": msg.ID,
		},
	})
	if _, err := result.Get(ctx); err != nil {
		return fmt.Errorf("publish dead-letter: %w", err)
	}
	return nil
}

func runHealthServer(ctx context.Context, port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("health server stopped with error: %v", err)
	}
}
