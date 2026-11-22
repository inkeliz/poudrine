package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
)

func reloadCandidate(ctx context.Context, mode string, current config.Options) (config.Options, error) {
	if mode != "serve" && os.Geteuid() == 0 {
		if err := config.CheckRootConfigPath(current.ConfigPath); err != nil {
			return config.Options{}, err
		}
	}
	cfg, revision, err := config.LoadConfiguration(current.ConfigPath)
	if err != nil {
		return config.Options{}, err
	}
	if current.ConfigRevision == revision || reflect.DeepEqual(cfg, current.ConfigSettings) {
		current.ConfigRevision = revision
		return current, nil
	}
	updated, err := cfg.Options(current.ConfigPath)
	if err != nil {
		return config.Options{}, err
	}
	updated.ConfigRevision = revision
	if cfg.SameGeneration(current.ConfigSettings) {
		return updated, nil
	}
	return updated, config.PreflightConfiguration(ctx, mode, updated)
}

type configTransition struct {
	current config.Options
	next    *config.Options
}

func watchConfiguration(ctx context.Context, mode string, current config.Options, registered <-chan *builder.Builder, ticks <-chan time.Time, repoPolicy chan<- config.PackageOverlays) configTransition {
	var lastError string
	var worker *builder.Builder
	checked := current
	for {
		select {
		case <-ctx.Done():
			return configTransition{current: current}
		case worker = <-registered:
			worker.SetWorkingHours(current.Builder.WorkingHours)
		case <-ticks:
			updated, err := reloadCandidate(ctx, mode, checked)
			if err != nil {
				if err.Error() != lastError {
					log.Printf("configuration change: %v (continuing with active settings)", err)
					lastError = err.Error()
				}
				continue
			}
			lastError = ""
			previousPolicy := checked.Builder.PackageOverlays
			checked = updated
			if worker != nil {
				worker.SetWorkingHours(updated.Builder.WorkingHours)
			}
			if !updated.ConfigSettings.SameGeneration(current.ConfigSettings) {
				// A manual edit may arrive just before the rc restart request.
				// Reserve an idle boundary before cancelling this generation.
				if mode != "run" || worker == nil || worker.ReserveConfigChange() {
					return configTransition{current: current, next: &updated}
				}
			} else {
				current = updated
			}
			// An active build keeps its make settings. The repository must
			// enforce the new policy even if that build takes hours to finish.
			if repoPolicy != nil && !previousPolicy.Equal(updated.Builder.PackageOverlays) {
				select {
				case repoPolicy <- updated.Builder.PackageOverlays:
				case <-ctx.Done():
					return configTransition{current: current}
				}
			}
		}
	}
}

func runConfigured(ctx context.Context, mode string, initial config.Options) error {
	active := initial
	var fallback *config.Options
	var restore *builder.WorkerCheckpoint
	if mode == "run" {
		var err error
		if restore, err = readServiceCheckpoint(initial); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		generation, stop := context.WithCancel(ctx)
		registered := make(chan *builder.Builder, 1)
		repoPolicy := make(chan config.PackageOverlays)
		done := make(chan configTransition, 1)
		go func(current config.Options) {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			done <- watchConfiguration(generation, mode, current, registered, t.C, repoPolicy)
			stop()
		}(active)
		var err error
		if mode == "serve" {
			err = serve(generation, active)
		} else {
			err = supervise(generation, mode, active, registered, restore, repoPolicy)
			restore = nil
		}
		stop()
		change := <-done
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var upgrade *serviceUpgrade
		if errors.As(err, &upgrade) {
			restore = upgrade.worker.Checkpoint()
			if err := upgrade.exec(active, restore); err != nil {
				log.Printf("executing updated Poudrine: %v; restarting the existing supervisor with its saved queue", err)
			}
			continue
		}
		if change.next != nil {
			fallback = &change.current
			active = *change.next
			log.Printf("applying %s", active.ConfigPath)
			continue
		}
		if err != nil && fallback != nil {
			log.Printf("could not run new settings: %v; restoring previous running settings with the current package overlays", err)
			// A failure applying a listener or TLS change must not expose
			// stock packages that the administrator has just restricted.
			cfg := fallback.ConfigSettings
			cfg.PackageOverlays = active.Builder.PackageOverlays
			restored, restoreErr := cfg.Options(fallback.ConfigPath)
			if restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
			restored.ConfigRevision = change.current.ConfigRevision
			active = restored
			fallback = nil
			continue
		}
		return err
	}
	return ctx.Err()
}

func childSettings(path string) (config.Options, error) {
	cfg, err := config.DecodeConfiguration([]byte(os.Getenv("POUDRINE_CONFIG_SNAPSHOT")), path)
	if err != nil {
		return config.Options{}, fmt.Errorf("read supervisor settings: %w", err)
	}
	return cfg.Options(path)
}
