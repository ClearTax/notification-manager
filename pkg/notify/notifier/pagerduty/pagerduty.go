package pagerduty

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/log/level"
	"github.com/kubesphere/notification-manager/pkg/controller"
	"github.com/kubesphere/notification-manager/pkg/internal"
	"github.com/kubesphere/notification-manager/pkg/internal/pagerduty"
	"github.com/kubesphere/notification-manager/pkg/notify/notifier"
	"github.com/kubesphere/notification-manager/pkg/template"
	"github.com/kubesphere/notification-manager/pkg/utils"
	"github.com/prometheus/common/model"
)

const (
	DefaultSendTimeout = time.Second * 5

	maxEventSize       int = 512000
	maxSummaryLenRunes     = 1024

	truncationMarker = "…"

	pagerDutyEventURL     = "https://events.pagerduty.com/v2/enqueue"
	pagerDutyEventTrigger = "trigger"
	pagerDutyEventResolve = "resolve"

	alertSummaryKey = "summary"
)

type pagerDutyPayload struct {
	Summary       string            `json:"summary"`
	Source        string            `json:"source"`
	Severity      string            `json:"severity"`
	Timestamp     string            `json:"timestamp,omitempty"`
	Class         string            `json:"class,omitempty"`
	Component     string            `json:"component,omitempty"`
	Group         string            `json:"group,omitempty"`
	CustomDetails map[string]string `json:"custom_details,omitempty"`
}

type pagerDutyMessage struct {
	RoutingKey  string            `json:"routing_key,omitempty"`
	DedupKey    string            `json:"dedup_key,omitempty"`
	EventType   string            `json:"event_type,omitempty"`
	Description string            `json:"description,omitempty"`
	EventAction string            `json:"event_action"`
	Payload     *pagerDutyPayload `json:"payload"`
	Client      string            `json:"client,omitempty"`
	ClientURL   string            `json:"client_url,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
}

type pagerDutyResponse struct {
	Status   string `json:"status,omitempty"`
	Message  string `json:"message,omitempty"`
	DedupKey string `json:"dedup_key,omitempty"`
}

type Notifier struct {
	notifierCtl *controller.Controller
	receiver    *pagerduty.Receiver
	timeout     time.Duration
	logger      log.Logger
	routingKey  string

	sentSuccessfulHandler *func([]*template.Alert)
}

func NewPagerDutyNotifier(logger log.Logger, receiver internal.Receiver, notifierCtl *controller.Controller) (notifier.Notifier, error) {
	n := &Notifier{
		notifierCtl: notifierCtl,
		timeout:     DefaultSendTimeout,
		logger:      logger,
	}

	opts := notifierCtl.ReceiverOpts
	if opts != nil && opts.PagerDuty != nil {
		if opts.PagerDuty.RoutingKey != "" {
			n.routingKey = opts.PagerDuty.RoutingKey
		}
	}
	n.receiver = receiver.(*pagerduty.Receiver)

	return n, nil
}

func (n *Notifier) Notify(ctx context.Context, data *template.Data) error {
	alert := data.Alerts[0]

	labelSet := convertKVToCommonLabelSet(data.GroupLabels)
	key := labelSet.Fingerprint()

	summary, truncated := truncateInRunes(alert.Annotations[alertSummaryKey], maxSummaryLenRunes)
	if truncated {
		level.Warn(n.logger).Log("msg", "Truncated summary", "key", key, "max_runes", maxSummaryLenRunes)
	}

	var eventType = pagerDutyEventTrigger
	if model.AlertStatus(data.Status()) == model.AlertResolved {
		eventType = pagerDutyEventResolve
	}

	msg := &pagerDutyMessage{
		RoutingKey:  n.routingKey,
		EventAction: eventType,
		DedupKey:    key.String(),
		Details:     data.CommonLabels,
		Payload: &pagerDutyPayload{
			Summary:       summary,
			Source:        data.CommonLabels["alertname"],
			Severity:      alert.Labels["severity"],
			CustomDetails: alert.Labels,
		},
	}

	var buf bytes.Buffer
	if err := utils.JsonEncode(&buf, msg); err != nil {
		level.Error(n.logger).Log("msg", "PagerDutyNotifier: encode message error", "routingKey", n.routingKey, "error", err.Error())
		return err
	}

	request, err := http.NewRequest(http.MethodPost, pagerDutyEventURL, &buf)
	if err != nil {
		return err
	}

	body, err := utils.DoHttpRequest(ctx, nil, request.WithContext(ctx))
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "PagerDutyNotifier: do http error", "routingKey", n.routingKey, "error", err)
		return err
	}

	var resp pagerDutyResponse
	if err := utils.JsonUnmarshal(body, &resp); err != nil {
		_ = level.Error(n.logger).Log("msg", "PagerDutyNotifier: decode response body error", "routingKey", n.routingKey, "error", err)
		return err
	}

	if resp.Status != "success" {
		level.Error(n.logger).Log("msg", "PagerDutyNotifier: send message error", "routingKey", n.routingKey, "error", resp.Message)
		return utils.Error(resp.Message)
	}

	level.Debug(n.logger).Log("msg", "PagerDutyNotifier: send message", "routingKey", n.routingKey)

	return nil
}

func (n *Notifier) SetSentSuccessfulHandler(h *func([]*template.Alert)) {
	n.sentSuccessfulHandler = h
}

func truncateInRunes(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}

	if n <= 3 {
		return string(r[:n]), true
	}

	return string(r[:n-1]) + truncationMarker, true
}

func convertKVToCommonLabelSet(cls template.KV) model.LabelSet {
	mls := make(model.LabelSet, len(cls))
	for ln, lv := range cls {
		mls[model.LabelName(ln)] = model.LabelValue(lv)
	}
	return mls
}
