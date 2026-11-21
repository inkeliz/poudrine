package poudrine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"time"
)

// Check what can be verified before interrupting a running build.
func preflightConfiguration(ctx context.Context, mode string, o options) error {
	if mode != "serve" && os.Geteuid() == 0 {
		if err := o.checkWorkerStorage(mode); err != nil {
			return err
		}
	}
	if mode == "worker" {
		return nil
	}
	return validateWebTLS(ctx, o, mode == "run")
}

func reloadCandidate(ctx context.Context, mode string, current options) (options, error) {
	if mode != "serve" && os.Geteuid() == 0 {
		if err := checkRootConfigPath(current.configPath); err != nil {
			return options{}, err
		}
	}
	cfg, revision, err := loadConfiguration(current.configPath)
	if err != nil {
		return options{}, err
	}
	if current.configRevision == revision || reflect.DeepEqual(cfg, current.configSettings) {
		current.configRevision = revision
		return current, nil
	}
	updated, err := cfg.options(current.configPath)
	if err != nil {
		return options{}, err
	}
	updated.configRevision = revision
	if cfg.sameGeneration(current.configSettings) {
		return updated, nil
	}
	return updated, preflightConfiguration(ctx, mode, updated)
}

// Working hours are applied directly to an active worker. Overlay builds
// require a new worker generation, even when HTTP already enforces the policy.
func (c Configuration) sameGeneration(other Configuration) bool {
	c.WorkingHours = other.WorkingHours
	overlays := c.PackageOverlays
	c.PackageOverlays = other.PackageOverlays
	return reflect.DeepEqual(c, other) && overlays.equal(other.PackageOverlays)
}

type configTransition struct {
	current options
	next    *options
}

func watchConfiguration(ctx context.Context, mode string, current options, registered <-chan *Builder, ticks <-chan time.Time, repoPolicy chan<- PackageOverlays) configTransition {
	var lastError string
	var worker *Builder
	checked := current
	for {
		select {
		case <-ctx.Done():
			return configTransition{current: current}
		case worker = <-registered:
			worker.SetWorkingHours(current.builder.WorkingHours)
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
			previousPolicy := checked.builder.PackageOverlays
			checked = updated
			if worker != nil {
				worker.SetWorkingHours(updated.builder.WorkingHours)
			}
			if !updated.configSettings.sameGeneration(current.configSettings) {
				// A manual edit may arrive just before the rc restart request.
				// Reserve an idle boundary before cancelling this generation.
				if mode != "run" || worker == nil || worker.reserveConfigChange() {
					return configTransition{current: current, next: &updated}
				}
			} else {
				current = updated
			}
			// An active build keeps its make settings. The repository must
			// enforce the new policy even if that build takes hours to finish.
			if repoPolicy != nil && !previousPolicy.equal(updated.builder.PackageOverlays) {
				select {
				case repoPolicy <- updated.builder.PackageOverlays:
				case <-ctx.Done():
					return configTransition{current: current}
				}
			}
		}
	}
}

func runConfigured(ctx context.Context, mode string, initial options) error {
	active := initial
	var fallback *options
	var restore *workerCheckpoint
	if mode == "run" {
		var err error
		if restore, err = readServiceCheckpoint(initial); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		generation, stop := context.WithCancel(ctx)
		registered := make(chan *Builder, 1)
		repoPolicy := make(chan PackageOverlays)
		done := make(chan configTransition, 1)
		go func(current options) {
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
			restore = upgrade.worker.checkpoint()
			if err := upgrade.exec(active, restore); err != nil {
				log.Printf("executing updated Poudrine: %v; restarting the existing supervisor with its saved queue", err)
			}
			continue
		}
		if change.next != nil {
			fallback = &change.current
			active = *change.next
			log.Printf("applying %s", active.configPath)
			continue
		}
		if err != nil && fallback != nil {
			log.Printf("could not run new settings: %v; restoring previous running settings with the current package overlays", err)
			// A failure applying a listener or TLS change must not expose
			// stock packages that the administrator has just restricted.
			cfg := fallback.configSettings
			cfg.PackageOverlays = active.builder.PackageOverlays
			restored, restoreErr := cfg.options(fallback.configPath)
			if restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
			restored.configRevision = change.current.configRevision
			active = restored
			fallback = nil
			continue
		}
		return err
	}
	return ctx.Err()
}

func childSettings(path string) (options, error) {
	cfg, err := decodeConfiguration([]byte(os.Getenv("POUDRINE_CONFIG_SNAPSHOT")), path)
	if err != nil {
		return options{}, fmt.Errorf("read supervisor settings: %w", err)
	}
	return cfg.options(path)
}

func inventoryQuery(ctx context.Context, mode, dir string) error {
	if mode == "inventory-key" {
		if _, err := os.Stat(dir + "/inventory.sqlite"); errors.Is(err, os.ErrNotExist) {
			return json.NewEncoder(os.Stdout).Encode(nil)
		}
	}
	db, err := openInventory(dir, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if mode == "inventory-key" {
		var hasTable bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='admin_identity')").Scan(&hasTable); err != nil {
			return err
		}
		var key []byte
		if hasTable {
			err = db.QueryRowContext(ctx, "SELECT public_key FROM admin_identity WHERE id=1").Scan(&key)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(key)
	}
	origins, err := (inventory{db}).origins(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(origins)
}
