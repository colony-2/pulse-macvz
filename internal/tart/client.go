// Package tart retains the Tart constructor over the shared local lifecycle.
package tart

import (
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/local"
)

type Client = local.Client
type DeliveryObservation = local.DeliveryObservation

var Err = local.Err
var ErrPlatform = local.ErrPlatform

func New(c config.Tart, common config.Config) (*Client, error) { return local.New(c, common) }
