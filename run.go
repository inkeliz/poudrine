// Package poudrine provides the command entry point.
package poudrine

import (
	"context"

	"github.com/inkeliz/poudrine/internal/service"
)

// Run executes the Poudrine command with the supplied arguments.
func Run(ctx context.Context, args []string) error {
	return service.Run(ctx, args)
}
