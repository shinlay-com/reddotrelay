package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"reddotrelay/internal/config"
	"reddotrelay/internal/store/sqlite"
)

// Backfill CLI is deliberately status-only; mutations belong to the online
// authenticated API so they cannot race a second SQLite writer.
func runBackfill(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("backfill action is required: list or get")
	}
	flags := flag.NewFlagSet("backfill "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "config.yaml", "configuration path")
	id := flags.String("id", "", "job ID")
	limit := flags.Int("limit", 50, "maximum jobs")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	storage, err := config.LoadStoragePath(*path)
	if err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, storage)
	if err != nil {
		return err
	}
	defer store.Close()
	encoder := json.NewEncoder(out)
	switch args[0] {
	case "list":
		if *limit < 1 || *limit > 200 {
			return errors.New("limit must be between 1 and 200")
		}
		jobs, err := store.ListBackfills(ctx, *limit, "")
		if err != nil {
			return err
		}
		return encoder.Encode(jobs)
	case "get":
		if *id == "" {
			return errors.New("id is required")
		}
		job, err := store.Backfill(ctx, *id)
		if err != nil {
			return err
		}
		return encoder.Encode(job)
	default:
		return errors.New("unknown backfill action: use list or get")
	}
}
