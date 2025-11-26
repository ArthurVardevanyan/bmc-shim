package backend

import (
	"context"
	"log"
)

type noop struct{}

func NewNoop() Backend { return &noop{} }

func (n *noop) PowerOn(ctx context.Context) error {
	log.Println("noop backend: PowerOn")
	return nil
}

func (n *noop) PowerOff(ctx context.Context) error {
	log.Println("noop backend: PowerOff")
	return nil
}
