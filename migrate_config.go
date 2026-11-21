package poudrine

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Used only on upgrade by rc.d. JSON field names mirror the legacy flags,
// allowing the original values (including a custom state directory) to move
// together into the new file before starting either service process.
func migrateConfiguration(path string, args []string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := checkRootConfigPath(path); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	c := defaultConfiguration()
	c.RepoURL = os.Getenv("POUDRINE_REPO_URL")
	c.WebURL = os.Getenv("POUDRINE_WEB_URL")
	flags := flag.NewFlagSet("migrate-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fields := reflect.ValueOf(&c).Elem()
	for i := 0; i < fields.NumField(); i++ {
		name := strings.ReplaceAll(fields.Type().Field(i).Tag.Get("json"), "_", "-")
		switch field := fields.Field(i).Addr().Interface().(type) {
		case *string:
			flags.StringVar(field, name, *field, "")
		case *int:
			flags.IntVar(field, name, *field, "")
		case *float64:
			flags.Float64Var(field, name, *field, "")
		}
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("migrate legacy service arguments: %w", err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected legacy service argument %q", flags.Arg(0))
	}
	// daemon -c ran the old service with / as its working directory.
	for _, p := range []*string{&c.State, &c.Fingerprints, &c.LocalRepo} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join("/", *p)
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = saveConfiguration(path, "", data)
	if errors.Is(err, ErrConfigConflict) {
		return nil
	}
	return err
}
