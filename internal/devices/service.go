package devices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/store"
)

const CookieName = "agentd_device"

var (
	ErrEnrollmentDenied = errors.New("invalid enrollment token")
	ErrPushDisabled     = errors.New("push notifications are disabled")
)

type Store interface {
	EnrollDevice(context.Context, string, store.DeviceRecord) error
	DeviceByTokenHash(context.Context, string, time.Time) (store.DeviceRecord, error)
	UpdateDevicePreferences(context.Context, string, bool, bool, bool) error
	UpsertPushSubscription(context.Context, store.PushSubscriptionRecord) error
	DeletePushSubscription(context.Context, string) error
	Devices(context.Context) ([]store.DeviceRecord, error)
	DeleteDevice(context.Context, string) error
	PushSubscriptions(context.Context, string) ([]store.PushSubscriptionRecord, error)
}

type Options struct {
	Enabled         bool
	CookieSecure    bool
	EnrollmentToken string
	PushEnabled     bool
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string
	PushSender      PushSender
	Logger          *slog.Logger
}

type Device struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	NotifyReady         bool      `json:"notifyReady"`
	NotifyInputRequired bool      `json:"notifyInputRequired"`
	NotifyFailed        bool      `json:"notifyFailed"`
	CreatedAt           time.Time `json:"createdAt"`
	LastSeenAt          time.Time `json:"lastSeenAt"`
}

type Service struct {
	store           Store
	enabled         bool
	cookieSecure    bool
	enrollmentToken []byte
	pushEnabled     bool
	vapidPublicKey  string
	pushSender      PushSender
	logger          *slog.Logger
	pushMu          sync.Mutex
	pushes          sync.WaitGroup
	closed          bool
}

type contextKey struct{}

func New(store Store, options Options) (*Service, error) {
	if store == nil {
		return nil, errors.New("device store is required")
	}
	if options.Enabled && len(options.EnrollmentToken) < 24 {
		return nil, errors.New("AGENTD_ENROLLMENT_TOKEN must contain at least 24 characters when auth is enabled")
	}
	if options.PushEnabled {
		if !options.Enabled {
			return nil, errors.New("push requires device authentication")
		}
		if options.VAPIDPublicKey == "" || options.VAPIDPrivateKey == "" {
			return nil, errors.New("AGENTD_VAPID_PUBLIC_KEY and AGENTD_VAPID_PRIVATE_KEY are required when push is enabled")
		}
		if options.VAPIDSubject == "" {
			return nil, errors.New("push VAPID subject is required when push is enabled")
		}
		if options.PushSender == nil {
			publicKey, publicErr := base64.RawURLEncoding.DecodeString(options.VAPIDPublicKey)
			privateKey, privateErr := base64.RawURLEncoding.DecodeString(options.VAPIDPrivateKey)
			if publicErr != nil || privateErr != nil || len(publicKey) != 65 || publicKey[0] != 4 || len(privateKey) != 32 {
				return nil, errors.New("Web Push VAPID keys are invalid; generate a new pair with agentd -generate-vapid-keys")
			}
			options.PushSender = NewWebPushSender(options.VAPIDPublicKey, options.VAPIDPrivateKey, options.VAPIDSubject)
		}
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Service{
		store:           store,
		enabled:         options.Enabled,
		cookieSecure:    options.CookieSecure,
		enrollmentToken: []byte(options.EnrollmentToken),
		pushEnabled:     options.PushEnabled,
		vapidPublicKey:  options.VAPIDPublicKey,
		pushSender:      options.PushSender,
		logger:          options.Logger,
	}, nil
}

func (s *Service) Enabled() bool { return s.enabled }

func (s *Service) PushEnabled() bool { return s.pushEnabled }

func (s *Service) VAPIDPublicKey() string { return s.vapidPublicKey }

func (s *Service) Enroll(ctx context.Context, name, enrollmentToken string) (Device, string, error) {
	if !s.enabled || subtle.ConstantTimeCompare([]byte(enrollmentToken), s.enrollmentToken) != 1 {
		return Device{}, "", ErrEnrollmentDenied
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Device{}, "", errors.New("device name is required")
	}
	id, err := randomID("dev_", 12)
	if err != nil {
		return Device{}, "", err
	}
	token, err := randomToken(32)
	if err != nil {
		return Device{}, "", err
	}
	now := time.Now().UTC()
	record := store.DeviceRecord{
		ID:                  id,
		Name:                name,
		TokenHash:           tokenHash(token),
		NotifyReady:         true,
		NotifyInputRequired: true,
		NotifyFailed:        true,
		CreatedAt:           now,
		LastSeenAt:          now,
	}
	if err := s.store.EnrollDevice(ctx, tokenHash(enrollmentToken), record); err != nil {
		return Device{}, "", err
	}
	return publicDevice(record), token, nil
}

func (s *Service) AuthenticateRequest(r *http.Request) (Device, bool, error) {
	if !s.enabled {
		return Device{}, false, nil
	}
	token, fromCookie := requestToken(r)
	if token == "" {
		return Device{}, false, sql.ErrNoRows
	}
	record, err := s.store.DeviceByTokenHash(r.Context(), tokenHash(token), time.Now().UTC())
	if err != nil {
		return Device{}, false, err
	}
	return publicDevice(record), fromCookie, nil
}

func (s *Service) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Service) UpdatePreferences(ctx context.Context, deviceID string, ready, inputRequired, failed bool) error {
	return s.store.UpdateDevicePreferences(ctx, deviceID, ready, inputRequired, failed)
}
func (s *Service) List(ctx context.Context) ([]Device, error) {
	records, err := s.store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(records))
	for _, record := range records {
		devices = append(devices, publicDevice(record))
	}
	return devices, nil
}

func (s *Service) Revoke(ctx context.Context, deviceID string) error {
	return s.store.DeleteDevice(ctx, deviceID)
}

func (s *Service) Subscribe(ctx context.Context, deviceID string, subscription PushSubscription) error {
	if !s.pushEnabled {
		return ErrPushDisabled
	}
	if err := validatePushSubscription(subscription); err != nil {
		return err
	}
	return s.store.UpsertPushSubscription(ctx, store.PushSubscriptionRecord{
		DeviceID: deviceID,
		Endpoint: subscription.Endpoint,
		P256dh:   subscription.Keys.P256dh,
		Auth:     subscription.Keys.Auth,
	})
}

func (s *Service) Unsubscribe(ctx context.Context, deviceID string) error {
	return s.store.DeletePushSubscription(ctx, deviceID)
}

func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.enabled || publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		device, fromCookie, err := s.AuthenticateRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "device authentication required")
			return
		}
		if fromCookie && changesState(r.Method) && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "same-origin request required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, device)))
	})
}

func FromContext(ctx context.Context) (Device, bool) {
	device, ok := ctx.Value(contextKey{}).(Device)
	return device, ok
}

func (s *Service) Notify(sessionID, category, title, body string) {
	s.NotifyTarget(sessionID, "/?session="+url.QueryEscape(sessionID), category, title, body)
}

func (s *Service) NotifyTarget(targetID, targetURL, category, title, body string) {
	if !s.pushEnabled {
		return
	}
	s.pushMu.Lock()
	if s.closed {
		s.pushMu.Unlock()
		return
	}
	s.pushes.Add(1)
	s.pushMu.Unlock()
	go func() {
		defer s.pushes.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		records, err := s.store.PushSubscriptions(ctx, category)
		if err != nil {
			s.logger.Error("load push subscriptions failed", "category", category, "error", err)
			return
		}
		payload, err := json.Marshal(map[string]string{
			"sessionId": targetID,
			"title":     title,
			"body":      body,
			"category":  category,
			"url":       targetURL,
		})
		if err != nil {
			s.logger.Error("encode push notification failed", "error", err)
			return
		}
		for _, record := range records {
			status, err := s.pushSender.Send(ctx, payload, PushSubscription{
				Endpoint: record.Endpoint,
				Keys:     PushKeys{P256dh: record.P256dh, Auth: record.Auth},
			}, category, pushTopic(targetID, category))
			if err != nil {
				s.logger.Error("push delivery failed", "deviceId", record.DeviceID, "category", category, "error", err)
				continue
			}
			if status == http.StatusNotFound || status == http.StatusGone {
				if err := s.store.DeletePushSubscription(ctx, record.DeviceID); err != nil {
					s.logger.Error("delete expired push subscription failed", "deviceId", record.DeviceID, "error", err)
				}
				continue
			}
			if status < http.StatusOK || status >= http.StatusMultipleChoices {
				s.logger.Error("push service rejected notification", "deviceId", record.DeviceID, "category", category, "status", status)
			}
		}
	}()
}

func (s *Service) Close() {
	s.pushMu.Lock()
	s.closed = true
	s.pushMu.Unlock()
	s.pushes.Wait()
}

func pushTopic(sessionID, category string) string {
	digest := sha256.Sum256([]byte(sessionID + "\x00" + category))
	return "agentd-" + hex.EncodeToString(digest[:12])
}

func publicDevice(record store.DeviceRecord) Device {
	return Device{
		ID:                  record.ID,
		Name:                record.Name,
		NotifyReady:         record.NotifyReady,
		NotifyInputRequired: record.NotifyInputRequired,
		NotifyFailed:        record.NotifyFailed,
		CreatedAt:           record.CreatedAt,
		LastSeenAt:          record.LastSeenAt,
	}
}

func requestToken(r *http.Request) (string, bool) {
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")), false
	}
	cookie, err := r.Cookie(CookieName)
	if err == nil {
		return cookie.Value, true
	}
	return "", false
}

func publicPath(path string) bool {
	if path == "/healthz" || path == "/api/v1/auth/status" || path == "/api/v1/devices/enroll" ||
		strings.HasPrefix(path, "/api/v1/workers/") {
		return true
	}
	return !strings.HasPrefix(path, "/api/") && path != "/mcp"
}

func changesState(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host && (parsed.Scheme == "https" || parsed.Scheme == "http")
}

func validatePushSubscription(subscription PushSubscription) error {
	parsed, err := url.Parse(subscription.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return errors.New("push endpoint must be an HTTPS browser push service URL")
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := host == "fcm.googleapis.com" || host == "updates.push.services.mozilla.com" ||
		host == "web.push.apple.com" || strings.HasSuffix(host, ".notify.windows.com")
	if !allowed {
		return fmt.Errorf("unsupported browser push service %q", host)
	}
	if subscription.Keys.P256dh == "" || subscription.Keys.Auth == "" {
		return errors.New("push subscription keys are required")
	}
	return nil
}

func tokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func randomID(prefix string, size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
func GenerateEnrollmentToken() (string, error) {
	return randomToken(32)
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
