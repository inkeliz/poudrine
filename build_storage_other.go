//go:build !freebsd

package poudrine

import "context"

// Poudriere worker storage is only available on FreeBSD.
func (*Builder) monitorStorage(context.Context) {}
