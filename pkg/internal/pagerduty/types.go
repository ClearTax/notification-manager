package pagerduty

import (
	"fmt"

	"github.com/kubesphere/notification-manager/apis/v2beta2"
	"github.com/kubesphere/notification-manager/pkg/constants"
	"github.com/kubesphere/notification-manager/pkg/internal"
)

type Config struct {
	*internal.Common
}

type Receiver struct {
	*internal.Common
	// `url` gives the location of the webhook, in standard URL form.
	RoutingKey string `json:"url,omitempty"`
	*Config
}

func NewReceiver(tenantID string, obj *v2beta2.Receiver) internal.Receiver {
	if obj.Spec.PagerDuty == nil {
		return nil
	}

	w := obj.Spec.PagerDuty
	r := &Receiver{
		Common: &internal.Common{
			Name:          obj.Name,
			TenantID:      tenantID,
			Type:          constants.PagerDuty,
			Labels:        obj.Labels,
			Enable:        w.Enabled,
			AlertSelector: w.AlertSelector,
		},
	}

	if w.RoutingKey != nil {
		r.RoutingKey = *w.RoutingKey
	}

	return r
}

func (r *Receiver) SetConfig(_ internal.Config) {
	return
}

func (r *Receiver) Validate() error {

	if len(r.RoutingKey) == 0 {
		return fmt.Errorf("pagerduty receiver: routingKey is nil")
	}

	return nil
}

func (r *Receiver) Clone() internal.Receiver {

	return &Receiver{
		Common:     r.Common.Clone(),
		RoutingKey: r.RoutingKey,
		Config:     r.Config,
	}
}

func NewConfig(_ *v2beta2.Config) internal.Config {
	return nil
}

func (c *Config) Validate() error {
	return nil
}

func (c *Config) Clone() internal.Config {

	return nil
}
