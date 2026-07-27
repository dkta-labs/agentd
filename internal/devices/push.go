package devices

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type PushKeys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

type PushSubscription struct {
	Endpoint       string   `json:"endpoint"`
	ExpirationTime *float64 `json:"expirationTime,omitempty"`
	Keys           PushKeys `json:"keys"`
}

type PushSender interface {
	Send(context.Context, []byte, PushSubscription, string, string) (int, error)
}

type WebPushSender struct {
	publicKey  string
	privateKey string
	subject    string
	client     *http.Client
}

func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	privateKey, publicKey, err = webpush.GenerateVAPIDKeys()
	return publicKey, privateKey, err
}

func NewWebPushSender(publicKey, privateKey, subject string) *WebPushSender {
	return &WebPushSender{
		publicKey:  publicKey,
		privateKey: privateKey,
		subject:    strings.TrimPrefix(subject, "mailto:"),
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (s *WebPushSender) Send(
	ctx context.Context,
	payload []byte,
	subscription PushSubscription,
	category string,
	topic string,
) (int, error) {
	urgency := webpush.UrgencyNormal
	if category == "input_required" || category == "failed" {
		urgency = webpush.UrgencyHigh
	}
	response, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: subscription.Endpoint,
		Keys: webpush.Keys{
			P256dh: subscription.Keys.P256dh,
			Auth:   subscription.Keys.Auth,
		},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.publicKey,
		VAPIDPrivateKey: s.privateKey,
		TTL:             60,
		Topic:           topic,
		Urgency:         urgency,
	})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}
