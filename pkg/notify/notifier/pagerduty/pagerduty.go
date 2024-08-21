package pagerduty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/log/level"
	"github.com/kubesphere/notification-manager/pkg/async"
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

	maxSummaryLenRunes = 1024

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

	sentSuccessfulHandler *func([]*template.Alert)
}

func NewPagerDutyNotifier(logger log.Logger, receiver internal.Receiver, notifierCtl *controller.Controller) (notifier.Notifier, error) {
	n := &Notifier{
		notifierCtl: notifierCtl,
		timeout:     DefaultSendTimeout,
		logger:      logger,
	}

	n.receiver = receiver.(*pagerduty.Receiver)

	opts := notifierCtl.ReceiverOpts
	if opts != nil && opts.PagerDuty != nil {
		if opts.PagerDuty.RoutingKey != "" {
			n.receiver.RoutingKey = opts.PagerDuty.RoutingKey
		}
	}

	return n, nil
}

func (n *Notifier) Notify(ctx context.Context, data *template.Data) error {
	send := func(alert *template.Alert) error {
		labelSet := convertKVToCommonLabelSet(alert.Labels)
		key := labelSet.Fingerprint()

		start := time.Now()
		defer func() {
			_ = level.Debug(n.logger).Log("msg", "PagerDutyNotifier: send message", "key", key, "used", time.Since(start).String())
		}()

		summary, truncated := truncateInRunes(alert.Annotations[alertSummaryKey], maxSummaryLenRunes)
		if truncated {
			level.Warn(n.logger).Log("msg", "PagerDutyNotifier: Truncated summary", "key", key, "max_runes", maxSummaryLenRunes)
		}

		var eventType = pagerDutyEventTrigger
		if model.AlertStatus(alert.Status) == model.AlertResolved {
			eventType = pagerDutyEventResolve
		}

		routingKey := n.receiver.RoutingKey

		msg := &pagerDutyMessage{
			RoutingKey:  routingKey,
			EventAction: eventType,
			DedupKey:    key.String(),
			Details:     alert.Labels,
			Payload: &pagerDutyPayload{
				Summary:       summary,
				Source:        data.CommonLabels["alertname"],
				Severity:      alert.Labels["severity"],
				CustomDetails: alert.Labels,
			},
		}

		var buf bytes.Buffer
		if err := utils.JsonEncode(&buf, msg); err != nil {
			level.Error(n.logger).Log("msg", "PagerDutyNotifier: encode message error", "key", key, "error", err.Error())
			return err
		}

		request, err := http.NewRequest(http.MethodPost, pagerDutyEventURL, &buf)
		if err != nil {
			return err
		}

		body, err := DoHttpRequest(ctx, nil, request.WithContext(ctx))
		if err != nil {
			level.Error(n.logger).Log("msg", "PagerDutyNotifier: do http error", "key", key, "error", err)
			return err
		}

		var resp pagerDutyResponse
		if err := utils.JsonUnmarshal(body, &resp); err != nil {
			level.Error(n.logger).Log("msg", "PagerDutyNotifier: decode response body error", "key", key, "error", err)
			return err
		}

		if resp.Status != "success" {
			level.Error(n.logger).Log("msg", "PagerDutyNotifier: send message error", "key", key, resp.Message)
			return utils.Error(resp.Message)
		}

		level.Debug(n.logger).Log("msg", "PagerDutyNotifier: send message", "key", key, "type", eventType, labelSet.String())
		return nil
	}

	group := async.NewGroup(ctx)
	for _, alert := range data.Alerts {
		group.Add(func(stopCh chan interface{}) {
			err := send(alert)
			if err == nil {
				if n.sentSuccessfulHandler != nil {
					(*n.sentSuccessfulHandler)(data.Alerts)
				}
			}
			stopCh <- err
		})
	}
	return group.Wait()
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
	for _, l := range cls.SortedPairs() {
		mls[model.LabelName(l.Name)] = model.LabelValue(l.Value)
	}
	return mls
}

func DoHttpRequest(ctx context.Context, client *http.Client, request *http.Request) ([]byte, error) {

	if client == nil {
		client = &http.Client{}
	}

	resp, err := client.Do(request.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusAccepted {
		msg := ""
		if len(body) > 0 {
			msg = string(body)
		}
		return body, fmt.Errorf("%d, %s", resp.StatusCode, msg)
	}

	return body, nil
}
