package public

import (
	"context"
	"log"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

type clientActivity struct {
	ip  string
	pkg *packageinfo.Package
}

// StartRecording starts one bounded, best-effort inventory writer. Call it
// before serving requests and stop it before closing the database. Neither a
// full queue nor a failed write may delay a package download.
func (s *Repository) StartRecording(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	s.activity = make(chan clientActivity, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case event := <-s.activity:
				writeCtx, end := context.WithTimeout(ctx, 5*time.Second)
				err := s.Store.Record(writeCtx, event.ip, event.pkg)
				end()
				if err != nil && ctx.Err() == nil {
					log.Printf("record repository activity: %v", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *Repository) recordActivity(ip string, p *packageinfo.Package) {
	event := clientActivity{ip: ip}
	if p != nil {
		copy := *p
		event.pkg = &copy
	}
	select {
	case s.activity <- event:
	default:
	}
}
