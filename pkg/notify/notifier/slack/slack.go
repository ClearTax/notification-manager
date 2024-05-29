package slack

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/kubesphere/notification-manager/pkg/async"
	"github.com/kubesphere/notification-manager/pkg/controller"
	"github.com/kubesphere/notification-manager/pkg/internal"
	"github.com/kubesphere/notification-manager/pkg/internal/slack"
	"github.com/kubesphere/notification-manager/pkg/notify/notifier"
	"github.com/kubesphere/notification-manager/pkg/template"
	"github.com/kubesphere/notification-manager/pkg/utils"
)

const (
	DefaultSendTimeout = time.Second * 3
	URL                = "https://slack.com/api/chat.postMessage"
	DefaultTemplate    = `{{ template "nm.default.text" . }}`
)

type Notifier struct {
	notifierCtl *controller.Controller
	receiver    *slack.Receiver
	timeout     time.Duration
	logger      log.Logger
	tmpl        *template.Template

	sentSuccessfulHandler *func([]*template.Alert)
}

type slackRequest struct {
	Channel     string       `json:"channel"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	Title  string `json:"title,omitempty"`
	Text   string `json:"text,omitempty"`
	Footer string `json:"footer,omitempty"`
	Color  string `json:"color,omitempty"`
}

type slackResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func NewSlackNotifier(logger log.Logger, receiver internal.Receiver, notifierCtl *controller.Controller) (notifier.Notifier, error) {

	n := &Notifier{
		notifierCtl: notifierCtl,
		timeout:     DefaultSendTimeout,
		logger:      logger,
	}

	opts := notifierCtl.ReceiverOpts
	tmplName := DefaultTemplate
	if opts != nil && opts.Global != nil && !utils.StringIsNil(opts.Global.Template) {
		tmplName = opts.Global.Template
	}

	var (
		titleTmplName string
		colorTmplName string
	)

	if opts != nil && opts.Slack != nil {

		if opts.Slack.NotificationTimeout != nil {
			n.timeout = time.Second * time.Duration(*opts.Slack.NotificationTimeout)
		}

		if !utils.StringIsNil(opts.Slack.Template) {
			tmplName = opts.Slack.Template
		}

		if !utils.StringIsNil(opts.Slack.TitleTemplate) {
			titleTmplName = opts.Slack.TitleTemplate
		}

		if !utils.StringIsNil(opts.Slack.ColorTemplate) {
			colorTmplName = opts.Slack.ColorTemplate
		}
	}

	n.receiver = receiver.(*slack.Receiver)
	if n.receiver.Config == nil {
		_ = level.Warn(logger).Log("msg", "SlackNotifier: ignore receiver because of empty config")
		return nil, utils.Error("ignore receiver because of empty config")
	}

	if utils.StringIsNil(n.receiver.TmplName) {
		n.receiver.TmplName = tmplName
	}

	if utils.StringIsNil(n.receiver.TitleTmplName) && !utils.StringIsNil(titleTmplName) {
		n.receiver.TitleTmplName = titleTmplName
	}

	if utils.StringIsNil(n.receiver.ColorTmplName) && !utils.StringIsNil(colorTmplName) {
		n.receiver.ColorTmplName = colorTmplName
	}

	var err error
	n.tmpl, err = notifierCtl.GetReceiverTmpl(n.receiver.TmplText)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "SlackNotifier: create receiver template error", "error", err.Error())
		return nil, err
	}

	return n, nil
}

func (n *Notifier) SetSentSuccessfulHandler(h *func([]*template.Alert)) {
	n.sentSuccessfulHandler = h
}

func (n *Notifier) Notify(ctx context.Context, data *template.Data) error {

	msg, err := n.tmpl.Text(n.receiver.TmplName, data)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "SlackNotifier: generate message error", "error", err.Error())
		return err
	}

	var titleMsg string
	if !utils.StringIsNil(n.receiver.TitleTmplName) {
		titleMsg, err = n.tmpl.Text(n.receiver.TitleTmplName, data)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: generate title message error", "error", err.Error())
			return err
		}

	}

	var colorFmt string
	if !utils.StringIsNil(n.receiver.ColorTmplName) {
		colorFmt, err = n.tmpl.Text(n.receiver.ColorTmplName, data)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: generate color format error", "error", err.Error())
			return err
		}
	}

	token, err := n.notifierCtl.GetCredential(n.receiver.Token)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "SlackNotifier: get token secret", "error", err.Error())
		return err
	}

	send := func(channel string) error {

		start := time.Now()
		defer func() {
			_ = level.Debug(n.logger).Log("msg", "SlackNotifier: send message", "channel", channel, "used", time.Since(start).String())
		}()

		att := &attachment{
			Text:  msg,
			Title: titleMsg,
			Color: colorFmt,
		}

		sr := &slackRequest{
			Channel:     channel,
			Attachments: []attachment{*att},
		}

		var buf bytes.Buffer
		if err := utils.JsonEncode(&buf, sr); err != nil {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: encode message error", "channel", channel, "error", err.Error())
			return err
		}

		request, err := http.NewRequest(http.MethodPost, URL, &buf)
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)

		body, err := utils.DoHttpRequest(ctx, nil, request.WithContext(ctx))
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: do http error", "channel", channel, "error", err)
			return err
		}

		var slResp slackResponse
		if err := utils.JsonUnmarshal(body, &slResp); err != nil {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: decode response body error", "channel", channel, "error", err)
			return err
		}

		if !slResp.OK {
			_ = level.Error(n.logger).Log("msg", "SlackNotifier: send message error", "channel", channel, "error", slResp.Error)
			return utils.Error(slResp.Error)
		}

		_ = level.Debug(n.logger).Log("msg", "SlackNotifier: send message", "channel", channel)

		return nil
	}

	group := async.NewGroup(ctx)
	for _, channel := range n.receiver.Channels {
		ch := channel
		group.Add(func(stopCh chan interface{}) {
			err := send(ch)
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
