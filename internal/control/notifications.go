package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const maxNotificationAttempts = 6

var publicHostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
var webhookCarrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

type NotificationChannel struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Enabled   bool           `json:"enabled"`
	Config    map[string]any `json:"config"`
	HasSecret bool           `json:"has_secret"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type notificationDelivery struct {
	ID               string
	Attempt          int
	Channel          NotificationChannel
	SecretCiphertext string
	Notification     domain.Notification
}

func insertNotificationDeliveriesTx(ctx context.Context, tx *sql.Tx, notificationID string, createdAt time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM notification_channels
		WHERE enabled=1 AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	channelIDs := make([]string, 0)
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			_ = rows.Close()
			return err
		}
		channelIDs = append(channelIDs, channelID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, channelID := range channelIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(
			id,notification_id,channel_id,status,next_attempt_at,created_at,updated_at)
			VALUES(?1,?2,?3,'pending',?4,?4,?4) ON CONFLICT(notification_id,channel_id) DO NOTHING`,
			uuid.NewString(), notificationID, channelID, createdAt.UTC()); err != nil {
			return err
		}
	}
	return nil
}

type NotificationDeliveryView struct {
	ID             string     `json:"id"`
	NotificationID string     `json:"notification_id"`
	ChannelID      string     `json:"channel_id"`
	ChannelName    string     `json:"channel_name"`
	Status         string     `json:"status"`
	AttemptCount   int        `json:"attempt_count"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LastError      string     `json:"last_error,omitempty"`
	ResponseCode   int        `json:"response_code,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type NotificationDeliveryAttempt struct {
	Attempt      int       `json:"attempt"`
	Succeeded    bool      `json:"succeeded"`
	ResponseCode int       `json:"response_code,omitempty"`
	Error        string    `json:"error,omitempty"`
	AttemptedAt  time.Time `json:"attempted_at"`
}

func validatePublicAddress(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, `/\\?#@`) {
		return errors.New("public_address must be a hostname or IP address without scheme, path, or port")
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		if ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")); ip != nil && strings.Contains(ip.String(), ":") {
			return nil
		}
		return errors.New("public_address contains an invalid IPv6 address")
	}
	if ip := net.ParseIP(value); ip != nil {
		if strings.Contains(value, ":") {
			return errors.New("IPv6 public_address must be enclosed in brackets")
		}
		return nil
	}
	if len(value) > 253 || !publicHostPattern.MatchString(value) || strings.Contains(value, "..") {
		return errors.New("public_address contains an invalid hostname")
	}
	return nil
}

func validateChannel(kind string, config map[string]any, secret string) error {
	for _, key := range []string{"secret", "password", "token", "authorization", "api_key"} {
		if _, exists := config[key]; exists {
			return fmt.Errorf("%s must be supplied through the encrypted secret field", key)
		}
	}
	switch kind {
	case "webhook":
		target := stringFromAny(config["url"])
		parsed, err := url.Parse(target)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return errors.New("webhook config.url must be an HTTPS URL without credentials, query, or fragment")
		}
		if ip, err := netip.ParseAddr(parsed.Hostname()); err == nil && !publicWebhookIP(ip) {
			return errors.New("webhook config.url must not target a private or local network address")
		}
		if len(secret) < 16 {
			return errors.New("webhook signing secret must be at least 16 characters")
		}
	case "smtp":
		host := strings.TrimSpace(stringFromAny(config["host"]))
		if host == "" || strings.ContainsAny(host, "\r\n") {
			return errors.New("SMTP host is required")
		}
		port := intFromAny(config["port"])
		if port == 0 {
			config["port"] = 587
		} else if port < 1 || port > 65535 {
			return errors.New("SMTP port must be between 1 and 65535")
		}
		from := strings.TrimSpace(stringFromAny(config["from"]))
		to := stringSlice(config["to"])
		if !validMailField(from) || len(to) == 0 {
			return errors.New("SMTP from and at least one recipient are required")
		}
		for _, recipient := range to {
			if !validMailField(recipient) {
				return errors.New("SMTP recipient is invalid")
			}
		}
	default:
		return errors.New("channel kind must be webhook or smtp")
	}
	return nil
}

func validMailField(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && strings.Contains(value, "@") && !strings.ContainsAny(value, "\r\n")
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			if value := strings.TrimSpace(stringFromAny(item)); value != "" {
				result = append(result, value)
			}
		}
		return result
	case string:
		result := make([]string, 0)
		for _, item := range strings.Split(values, ",") {
			if item = strings.TrimSpace(item); item != "" {
				result = append(result, item)
			}
		}
		return result
	}
	return nil
}

func (s *Store) CreateNotificationChannel(ctx context.Context, masterKey []byte, channel NotificationChannel, secret, createdBy string) (NotificationChannel, error) {
	channel.Name = strings.TrimSpace(channel.Name)
	channel.Kind = strings.ToLower(strings.TrimSpace(channel.Kind))
	if channel.Name == "" || len(channel.Name) > 128 {
		return NotificationChannel{}, errors.New("channel name must be between 1 and 128 characters")
	}
	if channel.Config == nil {
		channel.Config = map[string]any{}
	}
	if err := validateChannel(channel.Kind, channel.Config, secret); err != nil {
		return NotificationChannel{}, err
	}
	ciphertext := ""
	var err error
	if secret != "" {
		ciphertext, err = encryptSecret(masterKey, secret)
		if err != nil {
			return NotificationChannel{}, err
		}
	}
	channel.ID = uuid.NewString()
	configRaw, _ := json.Marshal(channel.Config)
	err = s.db.QueryRowContext(ctx, `INSERT INTO notification_channels(id,name,kind,enabled,config,secret_ciphertext,created_by)
		VALUES(?1,?2,?3,?4,?5,?6,?7) RETURNING created_at,updated_at`, channel.ID, channel.Name, channel.Kind,
		channel.Enabled, string(configRaw), ciphertext, nullableUUID(createdBy)).Scan(&channel.CreatedAt, &channel.UpdatedAt)
	if err != nil {
		if isSQLiteConstraint(err) {
			return NotificationChannel{}, ErrConflict
		}
		return NotificationChannel{}, err
	}
	channel.HasSecret = ciphertext != ""
	return channel, nil
}

func (s *Store) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,enabled,config,secret_ciphertext<>'',created_at,updated_at
		FROM notification_channels WHERE deleted_at IS NULL ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]NotificationChannel, 0)
	for rows.Next() {
		var item NotificationChannel
		var raw []byte
		if err := rows.Scan(&item.ID, &item.Name, &item.Kind, &item.Enabled, &raw, &item.HasSecret, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &item.Config)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) DeleteNotificationChannel(ctx context.Context, id string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tag, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=false,deleted_at=?2,updated_at=?2 WHERE id=?1 AND deleted_at IS NULL`, id, now)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='skipped',last_error='channel deleted',updated_at=?2
		WHERE channel_id=?1 AND status IN ('pending','delivering')`, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListNotificationDeliveries(ctx context.Context, limit int) ([]NotificationDeliveryView, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT delivery.id,delivery.notification_id,delivery.channel_id,channel.name,delivery.status,
		delivery.attempt_count,delivery.next_attempt_at,delivery.last_error,delivery.response_code,delivery.delivered_at,delivery.updated_at
		FROM notification_deliveries delivery JOIN notification_channels channel ON channel.id=delivery.channel_id
		ORDER BY delivery.updated_at DESC,delivery.id DESC LIMIT ?1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]NotificationDeliveryView, 0)
	for rows.Next() {
		var item NotificationDeliveryView
		if err := rows.Scan(&item.ID, &item.NotificationID, &item.ChannelID, &item.ChannelName, &item.Status,
			&item.AttemptCount, &item.NextAttemptAt, &item.LastError, &item.ResponseCode, &item.DeliveredAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) RetryNotificationDelivery(ctx context.Context, id string) error {
	now := time.Now().UTC()
	tag, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries SET status='pending',next_attempt_at=?2,last_error='',updated_at=?2
		WHERE id=?1 AND status IN ('failed','pending')`, id, now)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) NotificationDeliveryAttempts(ctx context.Context, id string) ([]NotificationDeliveryAttempt, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE id=?1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT attempt,succeeded,response_code,error,attempted_at
		FROM notification_delivery_attempts WHERE delivery_id=?1 ORDER BY attempt`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]NotificationDeliveryAttempt, 0)
	for rows.Next() {
		var item NotificationDeliveryAttempt
		if err := rows.Scan(&item.Attempt, &item.Succeeded, &item.ResponseCode, &item.Error, &item.AttemptedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) claimNotificationDeliveries(ctx context.Context, limit int) ([]notificationDelivery, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='pending',next_attempt_at=?1,updated_at=?1,
		last_error='delivery lease expired' WHERE status='delivering' AND updated_at<?2`, now, now.Add(-5*time.Minute))
	rows, err := tx.QueryContext(ctx, `SELECT delivery.id,delivery.attempt_count+1,channel.id,channel.name,channel.kind,channel.enabled,
		channel.config,channel.secret_ciphertext,channel.created_at,channel.updated_at,
		notification.id,notification.severity,notification.type,notification.title,notification.message,notification.resource,
		notification.metadata,notification.read_at,notification.created_at
		FROM notification_deliveries delivery
		JOIN notification_channels channel ON channel.id=delivery.channel_id
		JOIN notifications notification ON notification.id=delivery.notification_id
		WHERE delivery.status='pending' AND delivery.next_attempt_at<=?2
		ORDER BY delivery.next_attempt_at,delivery.id LIMIT ?1`, limit, now)
	if err != nil {
		return nil, err
	}
	items := make([]notificationDelivery, 0)
	for rows.Next() {
		var item notificationDelivery
		var channelRaw, notificationRaw []byte
		if err := rows.Scan(&item.ID, &item.Attempt, &item.Channel.ID, &item.Channel.Name, &item.Channel.Kind, &item.Channel.Enabled,
			&channelRaw, &item.SecretCiphertext, &item.Channel.CreatedAt, &item.Channel.UpdatedAt,
			&item.Notification.ID, &item.Notification.Severity, &item.Notification.Type, &item.Notification.Title,
			&item.Notification.Message, &item.Notification.Resource, &notificationRaw, &item.Notification.ReadAt,
			&item.Notification.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(channelRaw, &item.Channel.Config)
		_ = json.Unmarshal(notificationRaw, &item.Notification.Metadata)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='delivering',updated_at=?2 WHERE id=?1`, item.ID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

func retryDelay(attempt int) time.Duration {
	delays := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}
	if attempt <= 0 {
		return delays[0]
	}
	if attempt > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt-1]
}

func (s *Store) finishNotificationDelivery(ctx context.Context, item notificationDelivery, responseCode int, deliveryErr error) error {
	now := time.Now().UTC()
	succeeded := deliveryErr == nil
	message := ""
	if deliveryErr != nil {
		message = deliveryErr.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_delivery_attempts(delivery_id,attempt,succeeded,response_code,error)
		VALUES(?1,?2,?3,?4,?5) ON CONFLICT(delivery_id,attempt) DO NOTHING`, item.ID, item.Attempt, succeeded, responseCode, message); err != nil {
		return err
	}
	if succeeded {
		_, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='succeeded',attempt_count=?2,response_code=?3,
			last_error='',delivered_at=?4,updated_at=?4 WHERE id=?1 AND status='delivering'`, item.ID, item.Attempt, responseCode, now)
	} else if item.Attempt >= maxNotificationAttempts {
		_, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='failed',attempt_count=?2,response_code=?3,
			last_error=?4,updated_at=?5 WHERE id=?1 AND status='delivering'`, item.ID, item.Attempt, responseCode, message, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE notification_deliveries SET status='pending',attempt_count=?2,response_code=?3,
			last_error=?4,next_attempt_at=?5,updated_at=?6 WHERE id=?1 AND status='delivering'`,
			item.ID, item.Attempt, responseCode, message, now.Add(retryDelay(item.Attempt)), now)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func webhookSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func publicWebhookIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.Is4() && webhookCarrierGradeNAT.Contains(ip) {
		return false
	}
	return true
}

func webhookHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       15 * time.Second,
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(dialCtx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !publicWebhookIP(ip) {
					continue
				}
				return dialer.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, errors.New("webhook host resolves only to blocked network addresses")
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webhook redirects are not allowed")
		},
	}
}

func (s *Server) deliverNotification(ctx context.Context, item notificationDelivery) (int, error) {
	secret := ""
	var err error
	if item.SecretCiphertext != "" {
		secret, err = decryptSecret(s.config.MasterKey, item.SecretCiphertext)
		if err != nil {
			return 0, fmt.Errorf("decrypt channel secret: %w", err)
		}
	}
	switch item.Channel.Kind {
	case "webhook":
		body, _ := json.Marshal(map[string]any{"event": "notification.created", "notification": item.Notification})
		timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, stringFromAny(item.Channel.Config["url"]), bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-L4D2Panel-Timestamp", timestamp)
		req.Header.Set("X-L4D2Panel-Signature", webhookSignature(secret, timestamp, body))
		response, err := webhookHTTPClient().Do(req)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return response.StatusCode, fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
		}
		return response.StatusCode, nil
	case "smtp":
		return 0, sendSMTP(ctx, item.Channel.Config, secret, item.Notification)
	default:
		return 0, errors.New("unsupported notification channel")
	}
}

func sendSMTP(ctx context.Context, config map[string]any, password string, notification domain.Notification) error {
	host := strings.TrimSpace(stringFromAny(config["host"]))
	port := intFromAny(config["port"])
	if port == 0 {
		port = 587
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	_ = connection.SetDeadline(time.Now().Add(15 * time.Second))
	client, err := smtp.NewClient(connection, host)
	if err != nil {
		_ = connection.Close()
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("SMTP server does not advertise STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}); err != nil {
		return err
	}
	username := strings.TrimSpace(stringFromAny(config["username"]))
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			return err
		}
	}
	from := strings.TrimSpace(stringFromAny(config["from"]))
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range stringSlice(config["to"]) {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	subject := strings.ReplaceAll(strings.ReplaceAll("[L4D2 Panel] "+notification.Title, "\r", ""), "\n", "")
	body := "From: " + from + "\r\nTo: " + strings.Join(stringSlice(config["to"]), ", ") +
		"\r\nSubject: " + subject + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + notification.Message + "\r\n"
	if _, err := io.WriteString(w, body); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func (s *Server) processNotificationDeliveries(ctx context.Context) {
	items, err := s.store.claimNotificationDeliveries(ctx, 10)
	if err != nil {
		s.logger.Error("claim notification deliveries", "error", err)
		return
	}
	var group sync.WaitGroup
	for _, item := range items {
		item := item
		group.Add(1)
		go func() {
			defer group.Done()
			if !item.Channel.Enabled {
				_, _ = s.store.db.ExecContext(ctx, `UPDATE notification_deliveries SET status='skipped',last_error='channel disabled',updated_at=?2 WHERE id=?1`, item.ID, time.Now().UTC())
				return
			}
			deliveryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			status, deliveryErr := s.deliverNotification(deliveryCtx, item)
			cancel()
			if err := s.store.finishNotificationDelivery(ctx, item, status, deliveryErr); err != nil {
				s.logger.Error("finish notification delivery", "delivery_id", item.ID, "error", err)
			}
		}()
	}
	group.Wait()
}

func (s *Server) handleListNotificationChannels(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListNotificationChannels(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name    string         `json:"name"`
		Kind    string         `json:"kind"`
		Enabled *bool          `json:"enabled,omitempty"`
		Config  map[string]any `json:"config"`
		Secret  string         `json:"secret,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	principal := principalFromContext(r.Context())
	item, err := s.store.CreateNotificationChannel(r.Context(), s.config.MasterKey, NotificationChannel{
		Name: request.Name, Kind: request.Kind, Enabled: enabled, Config: request.Config,
	}, request.Secret, principal.User.ID)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "channel_exists", "a notification channel with this name already exists")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_notification_channel", err.Error())
		}
		return
	}
	s.audit(r, "notification_channel.created", "notification_channel", item.ID, map[string]any{"kind": item.Kind})
	writeJSON(w, http.StatusCreated, map[string]any{"channel": item})
}

func (s *Server) handleDeleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	if err := s.store.DeleteNotificationChannel(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "notification_channel.deleted", "notification_channel", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListNotificationDeliveries(r.Context(), parseLimit(r, 100))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleRetryNotificationDelivery(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "deliveryID")
	if err := s.store.RetryNotificationDelivery(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "notification_delivery.retried", "notification_delivery", id, nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleNotificationDeliveryAttempts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.NotificationDeliveryAttempts(r.Context(), chi.URLParam(r, "deliveryID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
