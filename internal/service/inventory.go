package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/inkeliz/poudrine/internal/database"
)

// The supervisor sends fixed-size batches to this unprivileged SQLite writer.
func importBuildRecord(ctx context.Context, dir string, input io.Reader) error {
	var records []database.BuildRecord
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&records); err != nil {
		return err
	}
	if len(records) == 0 || len(records) > database.HistoryBatchSize {
		return errors.New("invalid package build batch size")
	}
	db, err := database.Open(dir, false)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.RecordBuilds(ctx, records)
}

func inventoryQuery(ctx context.Context, mode, dir string) error {
	if mode == "inventory-key" {
		key, err := database.ReadAdminKey(ctx, dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(key)
	}
	db, err := database.Open(dir, true)
	if err != nil {
		return err
	}
	defer db.Close()
	origins, err := db.Origins(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(origins)
}
