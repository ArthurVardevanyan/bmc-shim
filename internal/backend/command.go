package backend

import (
	"context"
	"errors"
	"os/exec"
)

type command struct {
	onCmd  string
	offCmd string
}

func NewCommand(onCmd, offCmd string) (Backend, error) {
	if onCmd == "" || offCmd == "" {
		return nil, errors.New("command backend requires both --on-cmd and --off-cmd")
	}
	return &command{onCmd: onCmd, offCmd: offCmd}, nil
}

func (c *command) PowerOn(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sh", "-lc", c.onCmd)
	return cmd.Run()
}

func (c *command) PowerOff(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sh", "-lc", c.offCmd)
	return cmd.Run()
}

func (c *command) Ping(ctx context.Context) error {
	return nil
}
